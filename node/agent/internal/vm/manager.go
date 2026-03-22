package vm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AndrewBudd/boxcutter/node/agent/internal/config"
	"github.com/AndrewBudd/boxcutter/node/agent/internal/vmid"
)

const (
	kernelPath = "/var/lib/boxcutter/kernel/vmlinux"
	fixedMAC   = "AA:FC:00:00:00:01"
)

// Manager handles VM lifecycle operations.
type Manager struct {
	mu          sync.Mutex
	cfg         *config.Config
	vmid        *vmid.Client
	migratingMu sync.Mutex
	migratingSet map[string]bool // names of VMs currently being migrated
}

func NewManager(cfg *config.Config, vmidClient *vmid.Client) *Manager {
	return &Manager{cfg: cfg, vmid: vmidClient, migratingSet: make(map[string]bool)}
}

// StartMigration atomically checks if a VM is already migrating and marks it.
// Returns true if migration was started, false if already migrating.
// The targetAddr is stored in the marker file for crash recovery (split-brain detection).
func (m *Manager) StartMigration(name, targetAddr string) bool {
	m.migratingMu.Lock()
	defer m.migratingMu.Unlock()
	if m.migratingSet[name] {
		return false
	}
	m.migratingSet[name] = true
	vmDir := VMDir(name)
	SetMigrating(vmDir, true, targetAddr)
	return true
}

// EndMigration clears the migration marker for a VM.
func (m *Manager) EndMigration(name string) {
	m.migratingMu.Lock()
	defer m.migratingMu.Unlock()
	delete(m.migratingSet, name)
	vmDir := VMDir(name)
	SetMigrating(vmDir, false)
}

// IsMigratingVM checks if a VM is currently being migrated (in-memory check).
func (m *Manager) IsMigratingVM(name string) bool {
	m.migratingMu.Lock()
	defer m.migratingMu.Unlock()
	return m.migratingSet[name]
}

// BridgeIP returns this node's bridge IP from config.
func (m *Manager) BridgeIP() string {
	if m.cfg != nil {
		return m.cfg.Node.BridgeIP
	}
	return ""
}

// ProgressFunc is called with phase updates during VM creation.
type ProgressFunc func(phase, message string)

// CreateRequest is the API input for creating a VM.
type CreateRequest struct {
	Name           string   `json:"name"`
	Type           string   `json:"type,omitempty"`        // "firecracker" (default) or "qemu"
	Description    string   `json:"description,omitempty"` // user-provided description
	VCPU           int      `json:"vcpu,omitempty"`
	RAMMIB         int      `json:"ram_mib,omitempty"`
	Disk           string   `json:"disk,omitempty"`
	CloneURL       string   `json:"clone_url,omitempty"`
	CloneURLs      []string `json:"clone_urls,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	AuthorizedKeys []string `json:"authorized_keys,omitempty"`

	progressFn ProgressFunc `json:"-"`
}

// AllCloneURLs returns all clone URLs, merging single and plural fields.
func (r *CreateRequest) AllCloneURLs() []string {
	if len(r.CloneURLs) > 0 {
		return r.CloneURLs
	}
	if r.CloneURL != "" {
		return []string{r.CloneURL}
	}
	return nil
}

func (r *CreateRequest) SetProgress(fn ProgressFunc) {
	r.progressFn = fn
}

func (r *CreateRequest) progress(phase, message string) {
	if r.progressFn != nil {
		r.progressFn(phase, message)
	}
}

// CreateResponse is returned after creating + starting a VM.
type CreateResponse struct {
	Name        string `json:"name"`
	TailscaleIP string `json:"tailscale_ip,omitempty"`
	Mark        int    `json:"mark"`
	Mode        string `json:"mode"`
	Status      string `json:"status"`
}

// Create creates and starts a VM.
func (m *Manager) Create(req *CreateRequest) (*CreateResponse, error) {
	st, err := m.createSetup(req)
	if err != nil {
		return nil, err
	}

	// Start the VM (TAP, Firecracker launch) — still fast, no lock needed
	vmDir := VMDir(st.Name)
	resp, err := m.startVM(st, req.progress)
	if err != nil {
		CleanupSnapshot(vmDir)
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("starting VM: %w", err)
	}

	// Post-start: SSH wait, Tailscale, vmid, clone (slow, no lock needed)
	m.postStartVM(st, resp, req.progress)

	return resp, nil
}

// createSetup handles VM creation: validates, allocates resources (locked), then
// creates rootfs and prepares config (unlocked, I/O heavy).
func (m *Manager) createSetup(req *CreateRequest) (*VMState, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if req.VCPU == 0 {
		req.VCPU = m.cfg.VMDefaults.VCPU
	}
	if req.RAMMIB == 0 {
		req.RAMMIB = m.cfg.VMDefaults.RAMMIB
	}
	if req.Disk == "" {
		req.Disk = m.cfg.VMDefaults.Disk
	}
	if req.Mode == "" {
		req.Mode = m.cfg.VMDefaults.Mode
	}

	// Phase 1: Lock for validation, capacity check, mark allocation, state save.
	var st *VMState
	var goldenPath string
	err := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()

		// Check capacity: reject if adding this VM would exceed 90% of system RAM
		sysRAM := m.getSystemRAMMiB()
		if sysRAM > 0 {
			allocatedRAM := m.getAllocatedRAMMiB()
			if allocatedRAM+req.RAMMIB > sysRAM*90/100 {
				return &CapacityError{msg: "node is full"}
			}
		}

		vmDir := VMDir(req.Name)
		if _, err := os.Stat(vmDir); err == nil {
			return fmt.Errorf("VM '%s' already exists", req.Name)
		}

		goldenPath = m.cfg.Storage.GoldenLocalPath
		if _, err := os.Stat(goldenPath); err != nil {
			return fmt.Errorf("golden image not found at %s", goldenPath)
		}

		goldenVer := resolveGoldenVersion(goldenPath)
		os.MkdirAll(vmDir, 0755)

		existingMarks := m.collectExistingMarks()
		mark := AllocateMark(req.Name, existingMarks)
		tap := TAPName(req.Name)

		cloneURLs := req.AllCloneURLs()
		var githubRepos []string
		for _, u := range cloneURLs {
			if repo := parseRepoURL(u); repo != "" {
				githubRepos = append(githubRepos, repo)
			}
		}

		st = &VMState{
			Name:        req.Name,
			Type:        req.Type,
			Description: req.Description,
			VCPU:        req.VCPU,
			RAMMIB:      req.RAMMIB,
			Mark:        mark,
			Mode:        req.Mode,
			MAC:         fixedMAC,
			Disk:        req.Disk,
			TAP:         tap,
			Created:     time.Now().Format(time.RFC3339),
			CloneURL:    req.CloneURL,
			CloneURLs:   cloneURLs,
			GitHubRepo:  firstOrEmpty(githubRepos),
			GitHubRepos: githubRepos,
			GoldenVer:   goldenVer,
		}
		if err := SaveVMState(vmDir, st); err != nil {
			os.RemoveAll(vmDir)
			return err
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}

	// Phase 2: I/O-heavy work — no lock needed (vmDir exists, mark allocated).
	vmDir := VMDir(req.Name)
	req.progress("snapshot", "Creating disk...")
	if err := CreateRootfs(vmDir, goldenPath, req.Disk); err != nil {
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("creating rootfs: %w", err)
	}

	if st.Type == "qemu" {
		// QEMU VMs: prepare rootfs with kernel modules for Docker support,
		// and configure Tailscale for kernel networking (not userspace).
		m.prepareRootfsForQEMU(st)
	} else {
		if err := writeFirecrackerConfig(vmDir, st); err != nil {
			CleanupSnapshot(vmDir)
			os.RemoveAll(vmDir)
			return nil, err
		}
	}

	m.prepareRootfs(st, req.AuthorizedKeys)

	return st, nil
}

// Start starts an existing stopped VM.
func (m *Manager) Start(name string) (*CreateResponse, error) {
	if m.IsMigratingVM(name) || IsMigrating(VMDir(name)) {
		return nil, fmt.Errorf("VM '%s' is being migrated", name)
	}
	st, err := m.startSetup(name)
	if err != nil {
		return nil, err
	}

	resp, err := m.startVM(st, nil)
	if err != nil {
		return nil, err
	}

	m.postStartVM(st, resp, nil)
	return resp, nil
}

// startSetup handles the locked phase of starting a VM.
func (m *Manager) startSetup(name string) (*VMState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vmDir := VMDir(name)
	st, err := LoadVMState(vmDir)
	if err != nil {
		return nil, fmt.Errorf("VM '%s' not found", name)
	}

	if IsRunning(vmDir) {
		return nil, fmt.Errorf("VM '%s' is already running", name)
	}

	// Clean up stale TAP/rules from previous runs (idempotent)
	TeardownTAP(st.TAP, st.Mark)
	CleanupSnapshot(vmDir)

	// Ensure dm-snapshot is active (resolve golden version for this VM)
	goldenPath := m.goldenPathForVM(st)
	if err := EnsureSnapshot(vmDir, goldenPath); err != nil {
		return nil, fmt.Errorf("ensuring snapshot: %w", err)
	}

	// Regenerate FC config (may be missing after relocation, or have stale paths)
	if err := writeFirecrackerConfig(vmDir, st); err != nil {
		return nil, fmt.Errorf("writing firecracker config: %w", err)
	}

	return st, nil
}

// startVM sets up TAP networking and launches Firecracker. This is fast (sub-second)
// and safe to call with or without the Manager mutex held.
func (m *Manager) startVM(st *VMState, progress ProgressFunc) (*CreateResponse, error) {
	emit := func(phase, msg string) {
		if progress != nil {
			progress(phase, msg)
		}
	}
	vmDir := VMDir(st.Name)

	// Set up TAP + fwmark
	if err := SetupTAP(st.TAP, st.Mark); err != nil {
		return nil, fmt.Errorf("setting up TAP: %w", err)
	}

	// Paranoid mode rules
	if st.Mode == "paranoid" {
		if err := SetupParanoidMode(st.TAP); err != nil {
			TeardownTAP(st.TAP, st.Mark)
			return nil, fmt.Errorf("paranoid mode setup: %w", err)
		}
	}

	// Clean stale sockets
	os.Remove(filepath.Join(vmDir, "api.sock"))
	os.Remove(filepath.Join(vmDir, "vsock.sock"))

	var pid int
	if st.Type == "qemu" {
		emit("starting", "Starting QEMU VM...")
		launchedPID, err := launchQEMU(vmDir, st)
		if err != nil {
			TeardownTAP(st.TAP, st.Mark)
			return nil, fmt.Errorf("starting qemu: %w", err)
		}
		pid = launchedPID
	} else {
		emit("starting", "Starting Firecracker VM...")
		logFile, _ := os.Create(filepath.Join(vmDir, "firecracker.log"))
		cmd := exec.Command("firecracker",
			"--api-sock", filepath.Join(vmDir, "api.sock"),
			"--config-file", filepath.Join(vmDir, "fc-config.json"),
		)
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err := cmd.Start(); err != nil {
			logFile.Close()
			TeardownTAP(st.TAP, st.Mark)
			return nil, fmt.Errorf("starting firecracker: %w", err)
		}
		logFile.Close()
		pid = cmd.Process.Pid
		os.WriteFile(filepath.Join(vmDir, "firecracker.pid"),
			[]byte(fmt.Sprintf("%d", pid)), 0644)
		go cmd.Wait()
	}

	log.Printf("VM %s started (type=%s, PID %d, mark %d)", st.Name, st.Type, pid, st.Mark)

	return &CreateResponse{
		Name:   st.Name,
		Mark:   st.Mark,
		Mode:   st.Mode,
		Status: "running",
	}, nil
}

// postStartVM handles slow post-launch operations: SSH wait, Tailscale join,
// vmid registration, repo cloning. Called WITHOUT the Manager mutex.
func (m *Manager) postStartVM(st *VMState, resp *CreateResponse, progress ProgressFunc) {
	emit := func(phase, msg string) {
		if progress != nil {
			progress(phase, msg)
		}
	}
	vmDir := VMDir(st.Name)

	// Skip Tailscale/vmid for internal provision VMs
	if strings.HasPrefix(st.Name, "_") {
		return
	}

	// Wait for SSH
	emit("ssh", "Waiting for VM to boot...")
	sshKey := m.cfg.SSH.PrivateKeyPath
	if err := WaitForSSH(st.TAP, sshKey, 30*time.Second); err != nil {
		log.Printf("Warning: SSH not ready for %s: %v", st.Name, err)
	}

	// Check if VM was deleted/migrated while we waited
	if !IsRunning(vmDir) {
		log.Printf("VM %s no longer running after SSH wait, aborting post-start", st.Name)
		return
	}

	// Join Tailscale
	emit("tailscale", "Joining Tailscale network...")
	tsIP := m.joinTailscale(st)

	// Check again after Tailscale (another slow operation)
	if !IsRunning(vmDir) {
		log.Printf("VM %s no longer running after Tailscale join, aborting post-start", st.Name)
		return
	}

	// Register with vmid
	if m.vmid != nil {
		vmType := st.Type
		if vmType == "" {
			vmType = "firecracker"
		}
		m.vmid.Register(&vmid.RegisterRequest{
			VMID:        st.Name,
			VMType:      vmType,
			IP:          "10.0.0.2",
			Mark:        st.Mark,
			Mode:        st.Mode,
			GitHubRepo:  st.GitHubRepo,
			GitHubRepos: st.AllGitHubRepos(),
		})
	}

	// Paranoid mode: inject proxy env
	if st.Mode == "paranoid" {
		emit("paranoid", "Configuring paranoid mode...")
		m.injectProxyEnv(st)
	}

	// Clone repos if specified
	cloneUrls := st.AllCloneURLs()
	if len(cloneUrls) > 0 {
		emit("clone", fmt.Sprintf("Cloning %d repo(s)...", len(cloneUrls)))
		if err := m.cloneRepos(st); err != nil {
			emit("clone_failed", fmt.Sprintf("Warning: %s", err))
		}
	}

	// Update state and response with Tailscale IP
	if tsIP != "" {
		st.TailscaleIP = tsIP
		SaveVMState(vmDir, st)
		resp.TailscaleIP = tsIP
	}
}

// Stop stops a running VM.
func (m *Manager) Stop(name string) error {
	if m.IsMigratingVM(name) || IsMigrating(VMDir(name)) {
		return fmt.Errorf("VM '%s' is being migrated", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.stopVM(name)
}


func (m *Manager) stopVM(name string) error {
	vmDir := VMDir(name)
	st, err := LoadVMState(vmDir)
	if err != nil {
		return fmt.Errorf("VM '%s' not found", name)
	}

	pid := ReadPID(vmDir)
	if pid == 0 {
		log.Printf("stopVM %s: no running process found", name)
		return nil
	}

	// Check if process is alive at all
	if err := syscall.Kill(pid, 0); err != nil {
		log.Printf("stopVM %s: PID %d already dead (%v)", name, pid, err)
		goto cleanup
	}

	// Graceful shutdown — skip for migrating VMs since they're paused.
	if !IsMigrating(vmDir) {
		if st.Type == "qemu" {
			// QEMU: SIGTERM triggers ACPI shutdown
			syscall.Kill(pid, syscall.SIGTERM)
			for i := 0; i < 10; i++ {
				if err := syscall.Kill(pid, 0); err != nil {
					log.Printf("stopVM %s: PID %d exited after SIGTERM", name, pid)
					goto cleanup
				}
				time.Sleep(time.Second)
			}
		} else {
			// Firecracker: CtrlAltDel via API
			apiSock := filepath.Join(vmDir, "api.sock")
			if _, err := os.Stat(apiSock); err == nil {
				run("curl", "-s", "--unix-socket", apiSock,
					"-X", "PUT", "http://localhost/actions",
					"-H", "Content-Type: application/json",
					"-d", `{"action_type":"SendCtrlAltDel"}`)

				for i := 0; i < 10; i++ {
					if err := syscall.Kill(pid, 0); err != nil {
						log.Printf("stopVM %s: PID %d exited after CtrlAltDel", name, pid)
						goto cleanup
					}
					time.Sleep(time.Second)
				}
			}
		}
	}

	// Force kill if still running.
	if err := syscall.Kill(pid, 0); err == nil {
		log.Printf("stopVM %s: sending SIGKILL to PID %d", name, pid)
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			log.Printf("stopVM %s: syscall.Kill SIGKILL returned error: %v", name, err)
		}
		// Wait for process to fully exit so file handles and mmaps are released.
		for i := 0; i < 50; i++ { // up to 5 seconds
			if err := syscall.Kill(pid, 0); err != nil {
				log.Printf("stopVM %s: PID %d exited after SIGKILL (%d iterations)", name, pid, i+1)
				goto cleanup
			}
			time.Sleep(100 * time.Millisecond)
		}
		// Process survived SIGKILL via syscall — try kill command as fallback
		log.Printf("stopVM %s: PID %d survived syscall SIGKILL, trying kill -9 command", name, pid)
		exec.Command("kill", "-9", fmt.Sprint(pid)).Run()
		for i := 0; i < 20; i++ { // up to 2 more seconds
			if err := syscall.Kill(pid, 0); err != nil {
				log.Printf("stopVM %s: PID %d exited after kill -9", name, pid)
				goto cleanup
			}
			time.Sleep(100 * time.Millisecond)
		}
		// Last resort: check /proc to understand what state it's in
		if procStatus, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); readErr == nil {
			log.Printf("stopVM %s: CRITICAL — PID %d unkillable. /proc status:\n%s", name, pid, string(procStatus))
		} else {
			// /proc file gone means process actually exited despite kill(pid,0) succeeding
			log.Printf("stopVM %s: PID %d /proc gone (race with exit), treating as dead", name, pid)
		}
	}

cleanup:
	os.Remove(filepath.Join(vmDir, PIDFile(st.Type)))
	os.Remove(filepath.Join(vmDir, "api.sock"))

	if st.Mark != 0 {
		TeardownTAP(st.TAP, st.Mark)
	}

	CleanupSnapshot(vmDir)

	log.Printf("VM %s stopped", name)
	return nil
}

// Destroy destroys a VM completely.
func (m *Manager) Destroy(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	vmDir := VMDir(name)
	if _, err := os.Stat(vmDir); err != nil {
		return fmt.Errorf("VM '%s' not found", name)
	}

	// Reject destroy during active migration to prevent race conditions.
	// Destroying mid-migration leaves orphaned files on the target and causes
	// the migration goroutine to fail with confusing errors.
	if m.IsMigratingVM(name) || IsMigrating(vmDir) {
		return fmt.Errorf("VM '%s' is being migrated", name)
	}

	// Deregister from vmid
	if m.vmid != nil {
		m.vmid.Deregister(name)
	}

	// Stop if running
	if IsRunning(vmDir) {
		m.stopVM(name)
	}

	// Extra cleanup for any remaining dm/loop devices
	dmName := "bc-" + name
	run("dmsetup", "remove", dmName)
	ss, err := LoadSnapshotState(vmDir)
	if err == nil {
		run("losetup", "-d", ss.BaseLoop)
		run("losetup", "-d", ss.CowLoop)
	}

	os.RemoveAll(vmDir)
	os.RemoveAll(filepath.Join("/dev/shm", "bc-"+name))     // clean import tmpfs files
	os.RemoveAll(filepath.Join("/dev/shm", "bc-"+name+"-mig")) // clean snapshot tmpfs files
	log.Printf("VM %s destroyed", name)
	return nil
}

// Get returns the state and status of a VM.
// Status is always derived from reality (process + migration marker), never stored.
func (m *Manager) Get(name string) (*VMState, string, error) {
	vmDir := VMDir(name)
	st, err := LoadVMState(vmDir)
	if err != nil {
		return nil, "", fmt.Errorf("VM '%s' not found", name)
	}
	return st, DeriveStatus(vmDir), nil
}

// List returns all VMs with their status.
func (m *Manager) List() ([]map[string]interface{}, error) {
	vms, err := ListVMs()
	if err != nil {
		return nil, err
	}
	var result []map[string]interface{}
	for _, st := range vms {
		status := DeriveStatus(VMDir(st.Name))
		vmType := st.Type
		if vmType == "" {
			vmType = "firecracker"
		}
		result = append(result, map[string]interface{}{
			"name":         st.Name,
			"type":         vmType,
			"description":  st.Description,
			"tailscale_ip": st.TailscaleIP,
			"mark":         st.Mark,
			"mode":         st.Mode,
			"vcpu":         st.VCPU,
			"ram_mib":      st.RAMMIB,
			"disk":         st.Disk,
			"status":       status,
		})
	}
	return result, nil
}

// cleanupMigrationArtifacts kills orphaned SSH ControlMaster processes,
// removes stale migration socket files, and cleans up orphaned VM directories
// left by interrupted inbound migrations.
func cleanupMigrationArtifacts() {
	// Kill SSH ControlMaster connections from interrupted migrations.
	// These are background processes (ssh -fN -o ControlMaster=yes) that persist
	// after agent death because KillMode=process only kills the main Go process.
	//
	// Two cleanup passes:
	// 1. Close via control socket (graceful)
	// 2. Kill processes by pattern (catches cases where socket was already removed)
	sockets, _ := filepath.Glob("/tmp/bc-migrate-*")
	for _, sock := range sockets {
		vmName := strings.TrimPrefix(filepath.Base(sock), "bc-migrate-")
		exec.Command("ssh", "-o", "ControlPath="+sock, "-O", "exit", "dummy").Run()
		os.Remove(sock) // remove socket file regardless
		log.Printf("Cleaned up orphaned SSH ControlMaster socket for %s", vmName)
	}

	// Kill any remaining ssh ControlMaster processes by pattern.
	// These can outlive their socket files if the socket was cleaned up
	// by a successful migration's defer before the agent died.
	exec.Command("pkill", "-f", "ssh.*ControlPath=/tmp/bc-migrate-").Run()

	// Clean up orphaned VM directories from interrupted inbound migrations.
	// These have a rootfs.ext4 from pre-sync but no vm.json (import-snapshot
	// never completed). Safe to remove because the source VM was resumed.
	//
	// Guard: skip directories modified within the last 10 minutes — they may
	// belong to an active pre-sync from a remote source (Bug #88: agent restart
	// during drain deleted pre-synced files before import-snapshot arrived).
	vmBase := "/var/lib/boxcutter/vms"
	entries, _ := os.ReadDir(vmBase)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(vmBase, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "vm.json")); err == nil {
			continue // has state file — legitimate VM
		}
		// Check if directory was recently modified (active pre-sync protection)
		info, err := e.Info()
		if err == nil && time.Since(info.ModTime()) < 10*time.Minute {
			log.Printf("Skipping recent migration directory (age=%s): %s",
				time.Since(info.ModTime()).Round(time.Second), dir)
			continue
		}
		// No vm.json and old enough — this is an orphan from an interrupted migration
		log.Printf("Removing orphaned migration directory: %s", dir)
		os.RemoveAll(dir)
	}

	// Clean up orphaned /dev/shm directories for VMs that no longer exist.
	// These persist after VM destruction because Firecracker mmaps vm.mem from
	// /dev/shm — the unlinked file's tmpfs space stays until FC exits. After
	// the FC process is gone, the empty directory remains.
	shmEntries, _ := os.ReadDir("/dev/shm")
	for _, e := range shmEntries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "bc-") {
			continue
		}
		// Extract VM name: bc-<name> or bc-<name>-mig
		vmName := strings.TrimPrefix(e.Name(), "bc-")
		vmName = strings.TrimSuffix(vmName, "-mig")
		vmDir := filepath.Join(vmBase, vmName)
		if _, err := os.Stat(filepath.Join(vmDir, "vm.json")); err == nil {
			continue // VM still exists — keep its /dev/shm files
		}
		shmDir := filepath.Join("/dev/shm", e.Name())
		log.Printf("Removing orphaned /dev/shm directory: %s", shmDir)
		os.RemoveAll(shmDir)
	}
}

// RestartAll restarts all VMs found on disk. Called on node agent startup
// to recover VMs after a node reboot.
func (m *Manager) RestartAll() {
	// Kill orphaned SSH ControlMaster processes from interrupted migrations.
	// These survive agent restart (KillMode=process) because they're forked
	// background processes (ssh -fN). The control sockets also need cleanup.
	cleanupMigrationArtifacts()

	vms, err := ListVMs()
	if err != nil || len(vms) == 0 {
		return
	}

	log.Printf("Found %d VM(s) on disk, restarting...", len(vms))

	// Brief delay to let systemd finish killing orphaned Firecracker processes
	// from the previous agent run (they may still be alive during cgroup cleanup)
	time.Sleep(2 * time.Second)

	for _, st := range vms {
		vmDir := VMDir(st.Name)

		// If a migration was in progress when the agent died, the VM might
		// be paused with a stale "migrating" marker. Before resuming, check
		// if the target already has this VM running (split-brain prevention).
		// If agent crashed after import-snapshot but before stopVM, both copies
		// would be running — we must detect this and destroy the local copy.
		if IsMigrating(vmDir) && IsRunning(vmDir) {
			target := MigrationTarget(vmDir)
			if target != "" {
				log.Printf("  %s: stale migration marker found (target=%s), checking for split-brain", st.Name, target)
				targetHasVM := false
				client := &http.Client{Timeout: 5 * time.Second}
				checkResp, err := client.Get(fmt.Sprintf("http://%s/api/vms/%s", target, st.Name))
				if err == nil {
					var detail map[string]interface{}
					json.NewDecoder(checkResp.Body).Decode(&detail)
					checkResp.Body.Close()
					if s, _ := detail["status"].(string); s == "running" {
						targetHasVM = true
					}
				}
				if targetHasVM {
					// Target has a running copy — migration completed but source wasn't cleaned up.
					// Destroy the local (paused) copy to resolve split-brain.
					log.Printf("  %s: TARGET %s has running copy — destroying local (paused) copy to prevent split-brain", st.Name, target)
					m.stopVM(st.Name)
					CleanupSnapshot(vmDir)
					os.RemoveAll(vmDir)
					os.RemoveAll(filepath.Join("/dev/shm", "bc-"+st.Name+"-mig"))
					os.RemoveAll(filepath.Join("/dev/shm", "bc-"+st.Name))
					continue
				}
				log.Printf("  %s: target %s does not have running copy, safe to resume locally", st.Name, target)
			} else {
				log.Printf("  %s: stale migration marker found (no target info), resuming paused VM", st.Name)
			}
			if err := fcResume(vmDir); err != nil {
				log.Printf("  %s: resume failed (will restart from scratch): %v", st.Name, err)
			} else {
				SetMigrating(vmDir, false)
				os.RemoveAll(filepath.Join("/dev/shm", "bc-"+st.Name+"-mig"))
				log.Printf("  %s: resumed after interrupted migration", st.Name)
				continue
			}
		}
		SetMigrating(vmDir, false) // clear stale marker regardless

		if IsRunning(vmDir) {
			log.Printf("  %s: already running, skipping", st.Name)
			continue
		}

		goldenPath := m.goldenPathForVM(st)
		if _, err := os.Stat(goldenPath); err != nil {
			log.Printf("  %s: golden image %s not found, skipping", st.Name, st.GoldenVer)
			continue
		}

		// Clean up stale TAP/rules/snapshot from previous run
		TeardownTAP(st.TAP, st.Mark)
		CleanupSnapshot(vmDir)

		if err := EnsureSnapshot(vmDir, goldenPath); err != nil {
			log.Printf("  %s: snapshot failed: %v", st.Name, err)
			continue
		}

		if err := writeFirecrackerConfig(vmDir, st); err != nil {
			log.Printf("  %s: config failed: %v", st.Name, err)
			continue
		}

		resp, err := m.startVM(st, nil)
		if err != nil {
			log.Printf("  %s: start failed: %v", st.Name, err)
			continue
		}
		log.Printf("  %s: restarted (mark=%d, tailscale=%s)", st.Name, resp.Mark, resp.TailscaleIP)
	}
}

// GoldenPath returns the path to the golden rootfs image.
func (m *Manager) GoldenPath() string {
	return m.cfg.Storage.GoldenLocalPath
}

// GoldenDir returns the directory containing golden images.
func (m *Manager) GoldenDir() string {
	return filepath.Dir(m.cfg.Storage.GoldenLocalPath)
}

// goldenPathForVM resolves the golden image path for a specific VM's version.
func (m *Manager) goldenPathForVM(st *VMState) string {
	return GoldenPathForVersion(m.GoldenDir(), st.GoldenVer)
}

// GCGoldenImages removes golden images that no VM depends on,
// keeping the current version.
func (m *Manager) GCGoldenImages() []string {
	goldenDir := m.GoldenDir()
	current := resolveGoldenVersion(m.cfg.Storage.GoldenLocalPath)

	// Collect all versions in use by VMs
	vms, _ := ListVMs()
	inUse := make(map[string]bool)
	inUse[current] = true // always keep current
	for _, v := range vms {
		if v.GoldenVer != "" {
			inUse[v.GoldenVer] = true
		}
	}

	var removed []string
	for _, ver := range ListGoldenVersions(goldenDir) {
		if !inUse[ver] {
			path := filepath.Join(goldenDir, ver+".ext4")
			if os.Remove(path) == nil {
				removed = append(removed, ver)
				log.Printf("GC: removed unused golden image %s", ver)
			}
		}
	}
	return removed
}

// GoldenVersionsInUse returns the set of golden versions referenced by running VMs.
func (m *Manager) GoldenVersionsInUse() map[string]bool {
	vms, _ := ListVMs()
	inUse := make(map[string]bool)
	for _, v := range vms {
		if v.GoldenVer != "" {
			inUse[v.GoldenVer] = true
		}
	}
	return inUse
}

// Health returns node health and capacity info.
func (m *Manager) Health() map[string]interface{} {
	vms, _ := ListVMs()
	var running, totalRAM int
	for _, st := range vms {
		if IsRunning(VMDir(st.Name)) {
			running++
			totalRAM += st.RAMMIB
		}
	}

	hostname, _ := os.Hostname()

	// Get system RAM
	var sysRAM int
	out, err := runOutput("free", "-m")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "Mem:") {
				fmt.Sscanf(strings.Fields(line)[1], "%d", &sysRAM)
				break
			}
		}
	}

	// Get CPU count
	cpuCount := 0
	out, _ = runOutput("nproc")
	fmt.Sscanf(out, "%d", &cpuCount)

	// Check golden image
	goldenReady := false
	if _, err := os.Stat(m.cfg.Storage.GoldenLocalPath); err == nil {
		goldenReady = true
	}

	// CPU allocated across running VMs
	var allocatedVCPU int
	for _, st := range vms {
		if IsRunning(VMDir(st.Name)) {
			allocatedVCPU += st.VCPU
		}
	}

	// Disk usage
	var diskTotalMB, diskUsedMB int
	out, err = runOutput("df", "-BM", "--output=size,used", "/var/lib/boxcutter")
	if err == nil {
		lines := strings.Split(out, "\n")
		if len(lines) >= 2 {
			fmt.Sscanf(strings.ReplaceAll(lines[1], "M", ""), "%d %d", &diskTotalMB, &diskUsedMB)
		}
	}

	return map[string]interface{}{
		"hostname":          hostname,
		"vcpu_total":        cpuCount,
		"vcpu_allocated":    allocatedVCPU,
		"ram_total_mib":     sysRAM,
		"ram_allocated_mib": totalRAM,
		"ram_free_mib":      sysRAM - totalRAM,
		"disk_total_mb":     diskTotalMB,
		"disk_used_mb":      diskUsedMB,
		"vms_total":         len(vms),
		"vms_running":       running,
		"golden_ready":      goldenReady,
		"status":            "active",
	}
}

// --- Helpers ---

func (m *Manager) collectExistingMarks() map[int]bool {
	marks := make(map[int]bool)
	vms, _ := ListVMs()
	for _, st := range vms {
		if st.Mark != 0 {
			marks[st.Mark] = true
		}
	}
	return marks
}

func writeFirecrackerConfig(vmDir string, st *VMState) error {
	bootIP := fmt.Sprintf("ip=10.0.0.2::10.0.0.1:255.255.255.252:%s:eth0:off:8.8.8.8", st.Name)

	fcConfig := map[string]interface{}{
		"boot-source": map[string]string{
			"kernel_image_path": kernelPath,
			"boot_args":        fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on root=/dev/vda rw init=/sbin/init %s", bootIP),
		},
		"drives": []map[string]interface{}{
			{
				"drive_id":       "rootfs",
				"path_on_host":   RootfsPath(vmDir),
				"is_root_device": true,
				"is_read_only":   false,
			},
		},
		"network-interfaces": []map[string]string{
			{
				"iface_id":     "eth0",
				"guest_mac":    st.MAC,
				"host_dev_name": st.TAP,
			},
		},
		"machine-config": map[string]int{
			"vcpu_count":  st.VCPU,
			"mem_size_mib": st.RAMMIB,
		},
		"vsock": map[string]interface{}{
			"guest_cid": 3,
			"uds_path":  filepath.Join(vmDir, "vsock.sock"),
		},
		"entropy": map[string]interface{}{
			"rate_limiter": map[string]interface{}{
				"bandwidth": map[string]int{
					"size":        1048576,
					"refill_time": 1000,
				},
			},
		},
	}

	data, err := json.MarshalIndent(fcConfig, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(vmDir, "fc-config.json"), data, 0644)
}

// prepareRootfs mounts the rootfs once and injects CA cert + SSH keys.
func (m *Manager) prepareRootfs(st *VMState, authorizedKeys []string) {
	vmDir := VMDir(st.Name)
	mountDir, err := os.MkdirTemp("", "bc-mount-")
	if err != nil {
		return
	}
	defer os.RemoveAll(mountDir)

	if run("mount", RootfsPath(vmDir), mountDir) != nil {
		return
	}
	defer run("umount", mountDir)

	// Write VM type so processes inside can identify the hypervisor
	vmType := st.Type
	if vmType == "" {
		vmType = "firecracker"
	}
	bcDir := filepath.Join(mountDir, "etc", "boxcutter")
	os.MkdirAll(bcDir, 0755)
	os.WriteFile(filepath.Join(bcDir, "vm-type"), []byte(vmType+"\n"), 0644)

	// CA cert
	caCert := m.cfg.TLS.CACertPath
	if _, err := os.Stat(caCert); err == nil {
		caDir := filepath.Join(mountDir, "usr/local/share/ca-certificates")
		os.MkdirAll(caDir, 0755)
		run("cp", caCert, filepath.Join(caDir, "boxcutter-ca.crt"))
		run("chroot", mountDir, "update-ca-certificates")
	}

	// SSH keys
	sshDir := filepath.Join(mountDir, "home/dev/.ssh")
	os.MkdirAll(sshDir, 0700)
	existingKeysPath := filepath.Join(sshDir, "authorized_keys")
	existingKeys, _ := os.ReadFile(existingKeysPath)

	keySet := make(map[string]bool)
	for _, k := range strings.Split(string(existingKeys), "\n") {
		k = strings.TrimSpace(k)
		if k != "" {
			keySet[k] = true
		}
	}

	// Add keys from provided list or from config file
	if len(authorizedKeys) > 0 {
		for _, k := range authorizedKeys {
			k = strings.TrimSpace(k)
			if k != "" {
				keySet[k] = true
			}
		}
	} else if authKeysFile := m.cfg.SSH.AuthorizedKeysPath; authKeysFile != "" {
		if newKeys, err := os.ReadFile(authKeysFile); err == nil {
			for _, k := range strings.Split(string(newKeys), "\n") {
				k = strings.TrimSpace(k)
				if k != "" {
					keySet[k] = true
				}
			}
		}
	}

	var merged []string
	for k := range keySet {
		merged = append(merged, k)
	}
	os.WriteFile(existingKeysPath, []byte(strings.Join(merged, "\n")+"\n"), 0600)
	run("chown", "-R", "1000:1000", sshDir)
}

func (m *Manager) injectCACert(st *VMState) {
	caCert := m.cfg.TLS.CACertPath
	if _, err := os.Stat(caCert); err != nil {
		return
	}

	dmName := "bc-" + st.Name
	mountDir, err := os.MkdirTemp("", "bc-mount-")
	if err != nil {
		return
	}
	defer os.RemoveAll(mountDir)

	if run("mount", "/dev/mapper/"+dmName, mountDir) != nil {
		return
	}
	defer run("umount", mountDir)

	caDir := filepath.Join(mountDir, "usr/local/share/ca-certificates")
	os.MkdirAll(caDir, 0755)
	run("cp", caCert, filepath.Join(caDir, "boxcutter-ca.crt"))
	run("chroot", mountDir, "update-ca-certificates")
}

func (m *Manager) injectSSHKeys(st *VMState) {
	authKeys := m.cfg.SSH.AuthorizedKeysPath
	if _, err := os.Stat(authKeys); err != nil {
		return
	}

	dmName := "bc-" + st.Name
	mountDir, err := os.MkdirTemp("", "bc-mount-")
	if err != nil {
		return
	}
	defer os.RemoveAll(mountDir)

	if run("mount", "/dev/mapper/"+dmName, mountDir) != nil {
		return
	}
	defer run("umount", mountDir)

	sshDir := filepath.Join(mountDir, "home/dev/.ssh")
	os.MkdirAll(sshDir, 0700)

	existingKeysPath := filepath.Join(sshDir, "authorized_keys")
	existingKeys, _ := os.ReadFile(existingKeysPath)
	newKeys, _ := os.ReadFile(authKeys)

	// Merge keys (avoid duplicates)
	keySet := make(map[string]bool)
	for _, k := range strings.Split(string(existingKeys), "\n") {
		k = strings.TrimSpace(k)
		if k != "" {
			keySet[k] = true
		}
	}
	for _, k := range strings.Split(string(newKeys), "\n") {
		k = strings.TrimSpace(k)
		if k != "" {
			keySet[k] = true
		}
	}

	var merged []string
	for k := range keySet {
		merged = append(merged, k)
	}
	os.WriteFile(existingKeysPath, []byte(strings.Join(merged, "\n")+"\n"), 0600)

	run("chown", "-R", "1000:1000", sshDir)
}

func (m *Manager) joinTailscale(st *VMState) string {
	sshKey := m.cfg.SSH.PrivateKeyPath
	authkey, err := config.ReadSecret(m.cfg.Tailscale.VMAuthkeyFile)
	if err != nil || authkey == "" {
		log.Printf("Warning: no Tailscale VM auth key")
		return ""
	}

	VMSSH(st.TAP, sshKey,
		fmt.Sprintf("sudo tailscale up --authkey='%s' --hostname='%s'", authkey, st.Name))

	VMSSH(st.TAP, sshKey,
		"sudo tailscale serve --bg --tcp 22 tcp://localhost:22")

	// Wait for Tailscale IP
	for i := 0; i < 15; i++ {
		out, err := VMSSH(st.TAP, sshKey, "tailscale ip -4 2>/dev/null")
		ip := strings.TrimSpace(out)
		if err == nil && ip != "" {
			log.Printf("VM %s got Tailscale IP: %s", st.Name, ip)
			return ip
		}
		time.Sleep(2 * time.Second)
	}
	log.Printf("Warning: Tailscale IP not ready for %s", st.Name)
	return ""
}

func (m *Manager) injectProxyEnv(st *VMState) {
	sshKey := m.cfg.SSH.PrivateKeyPath
	proxyScript := `cat > /etc/profile.d/boxcutter-proxy.sh << 'PROXYEOF'
export HTTP_PROXY=http://10.0.0.1:8080
export HTTPS_PROXY=http://10.0.0.1:8080
export http_proxy=http://10.0.0.1:8080
export https_proxy=http://10.0.0.1:8080
export NO_PROXY=10.0.0.1,localhost,127.0.0.1
export no_proxy=10.0.0.1,localhost,127.0.0.1
PROXYEOF`
	VMSSH(st.TAP, sshKey, "sudo bash -c '"+proxyScript+"'")
}

func (m *Manager) cloneRepos(st *VMState) error {
	sshKey := m.cfg.SSH.PrivateKeyPath
	repos := st.AllGitHubRepos()
	cloneURLs := st.AllCloneURLs()

	// Try to get GitHub token (scoped to all repos)
	var ghToken string
	if m.vmid != nil && len(repos) > 0 {
		tok, err := m.vmid.MintGitHubToken(st.Name)
		if err == nil && tok.Token != "" {
			ghToken = tok.Token
		}
	}

	// Set up credential helper that fetches fresh tokens from metadata service.
	// This ensures tokens are refreshed when repo policy changes (repos add).
	if ghToken != "" {
		setupCmd := fmt.Sprintf(`mkdir -p ~/.config/gh
cat > ~/.config/gh/hosts.yml <<GHEOF
github.com:
    oauth_token: %s
    user: x-access-token
    git_protocol: https
GHEOF
git config --global credential.helper '!f() { token=$(curl -sf http://169.254.169.254/token/github 2>/dev/null | grep -o "\"token\":\"[^\"]*\"" | cut -d\" -f4); [ -n "$token" ] && { echo username=x-access-token; echo password=$token; } || { echo username=x-access-token; echo password=%s; }; }; f'`,
			ghToken, ghToken)
		VMSSH(st.TAP, sshKey, setupCmd)
	}

	// Determine clone target directory
	// Single repo → ~/project (backwards compat), multiple → ~/projects/<name>
	multiRepo := len(cloneURLs) > 1

	for i, rawURL := range cloneURLs {
		cloneURL := rawURL
		// Expand shorthand owner/repo to full GitHub URL
		if !strings.Contains(cloneURL, "://") && !strings.HasPrefix(cloneURL, "git@") {
			cloneURL = fmt.Sprintf("https://github.com/%s.git", cloneURL)
		}

		// Use token-authed URL for clone (will be stripped after)
		var repoName string
		if i < len(repos) {
			repoName = repos[i]
			if ghToken != "" {
				cloneURL = fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", ghToken, repoName)
			}
		}

		// Target directory
		targetDir := "~/project"
		if multiRepo {
			// Use the repo name (after the /) as directory name
			parts := strings.SplitN(repoName, "/", 2)
			dirName := repoName
			if len(parts) == 2 {
				dirName = parts[1]
			}
			if dirName == "" {
				dirName = fmt.Sprintf("repo-%d", i)
			}
			targetDir = fmt.Sprintf("~/projects/%s", dirName)
		}

		out, err := VMSSH(st.TAP, sshKey, fmt.Sprintf("git clone '%s' %s 2>&1", cloneURL, targetDir))
		if err != nil {
			log.Printf("Clone failed for %s repo %s: %s", st.Name, rawURL, out)
			continue // don't fail all clones if one fails
		}

		// Strip token from remote URL
		if ghToken != "" && repoName != "" {
			VMSSH(st.TAP, sshKey, fmt.Sprintf("cd %s && git remote set-url origin 'https://github.com/%s.git'", targetDir, repoName))
		}

		log.Printf("VM %s: cloned %s -> %s", st.Name, rawURL, targetDir)
	}

	// If multiple repos, symlink ~/project to first one for convenience
	if multiRepo && len(repos) > 0 {
		parts := strings.SplitN(repos[0], "/", 2)
		dirName := repos[0]
		if len(parts) == 2 {
			dirName = parts[1]
		}
		VMSSH(st.TAP, sshKey, fmt.Sprintf("ln -sf ~/projects/%s ~/project", dirName))
	}

	return nil
}

// CapacityError indicates the node cannot accept more VMs.
type CapacityError struct {
	msg string
}

func (e *CapacityError) Error() string { return e.msg }

// IsCapacityError returns true if the error is a capacity error.
func IsCapacityError(err error) bool {
	_, ok := err.(*CapacityError)
	return ok
}

func (m *Manager) getSystemRAMMiB() int {
	out, err := runOutput("free", "-m")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Mem:") {
			var ram int
			fmt.Sscanf(strings.Fields(line)[1], "%d", &ram)
			return ram
		}
	}
	return 0
}

func (m *Manager) getAllocatedRAMMiB() int {
	vms, _ := ListVMs()
	var total int
	for _, st := range vms {
		if IsRunning(VMDir(st.Name)) {
			total += st.RAMMIB
		}
	}
	return total
}

func (m *Manager) injectSSHKeysFromPath(st *VMState, authKeysPath string) {
	if _, err := os.Stat(authKeysPath); err != nil {
		return
	}

	dmName := "bc-" + st.Name
	mountDir, err := os.MkdirTemp("", "bc-mount-")
	if err != nil {
		return
	}
	defer os.RemoveAll(mountDir)

	if run("mount", "/dev/mapper/"+dmName, mountDir) != nil {
		return
	}
	defer run("umount", mountDir)

	sshDir := filepath.Join(mountDir, "home/dev/.ssh")
	os.MkdirAll(sshDir, 0700)

	existingKeysPath := filepath.Join(sshDir, "authorized_keys")
	existingKeys, _ := os.ReadFile(existingKeysPath)
	newKeys, _ := os.ReadFile(authKeysPath)

	// Merge keys (avoid duplicates)
	keySet := make(map[string]bool)
	for _, k := range strings.Split(string(existingKeys), "\n") {
		k = strings.TrimSpace(k)
		if k != "" {
			keySet[k] = true
		}
	}
	for _, k := range strings.Split(string(newKeys), "\n") {
		k = strings.TrimSpace(k)
		if k != "" {
			keySet[k] = true
		}
	}

	var merged []string
	for k := range keySet {
		merged = append(merged, k)
	}
	os.WriteFile(existingKeysPath, []byte(strings.Join(merged, "\n")+"\n"), 0600)

	run("chown", "-R", "1000:1000", sshDir)
}

// RunningVMCount returns the number of currently running VMs.
func (m *Manager) RunningVMCount() int {
	vms, _ := ListVMs()
	count := 0
	for _, st := range vms {
		if IsRunning(VMDir(st.Name)) {
			count++
		}
	}
	return count
}

// AllocatedRAMMiB returns total RAM allocated to running VMs.
func (m *Manager) AllocatedRAMMiB() int {
	return m.getAllocatedRAMMiB()
}

var repoURLPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^git@[^:]+:(.+?)(?:\.git)?$`),
	regexp.MustCompile(`^https?://[^/]+/(.+?)(?:\.git)?$`),
	regexp.MustCompile(`^[^/]+\.[^/]+/(.+?)(?:\.git)?$`),
}

func parseRepoURL(url string) string {
	for _, re := range repoURLPatterns {
		if m := re.FindStringSubmatch(url); len(m) > 1 {
			return m[1]
		}
	}
	return url
}

func firstOrEmpty(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

// AddRepo adds a GitHub repo to a VM's policy and registers it with vmid.
func (m *Manager) AddRepo(name, repo string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vmDir := VMDir(name)
	st, err := LoadVMState(vmDir)
	if err != nil {
		return nil, fmt.Errorf("VM '%s' not found", name)
	}

	// Update state
	repos := st.AllGitHubRepos()
	for _, r := range repos {
		if r == repo {
			return repos, nil // already present
		}
	}
	repos = append(repos, repo)
	st.GitHubRepos = repos
	st.GitHubRepo = firstOrEmpty(repos)
	if err := SaveVMState(vmDir, st); err != nil {
		return nil, err
	}

	// Update vmid registration
	if m.vmid != nil {
		m.vmid.AddRepo(name, repo)
	}

	// Refresh the GitHub token inside the VM so git picks up the new repo scope.
	// The credential helper fetches from the metadata service, which now includes
	// the new repo in its token scope.
	sshKey := m.cfg.SSH.PrivateKeyPath
	if sshKey == "" {
		sshKey = "/etc/boxcutter/secrets/cluster-ssh.key"
	}
	go func() {
		if IsRunning(vmDir) {
			VMSSH(st.TAP, sshKey, "/usr/local/bin/gh-token-refresh 2>/dev/null || true")
		}
	}()

	return repos, nil
}

// RemoveRepo removes a GitHub repo from a VM's policy.
func (m *Manager) RemoveRepo(name, repo string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vmDir := VMDir(name)
	st, err := LoadVMState(vmDir)
	if err != nil {
		return nil, fmt.Errorf("VM '%s' not found", name)
	}

	repos := st.AllGitHubRepos()
	found := false
	var newRepos []string
	for _, r := range repos {
		if r == repo {
			found = true
			continue
		}
		newRepos = append(newRepos, r)
	}
	if !found {
		return nil, fmt.Errorf("repo '%s' not in VM policy", repo)
	}

	st.GitHubRepos = newRepos
	st.GitHubRepo = firstOrEmpty(newRepos)
	if err := SaveVMState(vmDir, st); err != nil {
		return nil, err
	}

	if m.vmid != nil {
		m.vmid.RemoveRepo(name, repo)
	}

	return newRepos, nil
}

// ListRepos returns the GitHub repos configured for a VM.
func (m *Manager) ListRepos(name string) ([]string, error) {
	vmDir := VMDir(name)
	st, err := LoadVMState(vmDir)
	if err != nil {
		return nil, fmt.Errorf("VM '%s' not found", name)
	}
	return st.AllGitHubRepos(), nil
}

// ExportVM stops a VM and returns the path to its COW image for transfer.
// Used as fallback when snapshot-based migration isn't possible.
func (m *Manager) ExportVM(name string) (string, *VMState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vmDir := VMDir(name)
	st, err := LoadVMState(vmDir)
	if err != nil {
		return "", nil, fmt.Errorf("VM '%s' not found", name)
	}

	// Deregister from vmid
	if m.vmid != nil {
		m.vmid.Deregister(name)
	}

	// Stop if running
	if IsRunning(vmDir) {
		m.stopVM(name)
	}

	cowPath := filepath.Join(vmDir, "cow.img")
	if _, err := os.Stat(cowPath); err != nil {
		return "", nil, fmt.Errorf("COW image not found")
	}

	return cowPath, st, nil
}

// CopyVM creates a new VM by copying an existing VM's disk.
// The source VM is stopped during the copy, then restarted.
func (m *Manager) CopyVM(srcName, dstName string, progressFn ProgressFunc) (*CreateResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	srcDir := VMDir(srcName)
	srcSt, err := LoadVMState(srcDir)
	if err != nil {
		return nil, fmt.Errorf("source VM '%s' not found", srcName)
	}

	dstDir := VMDir(dstName)
	if _, err := os.Stat(dstDir); err == nil {
		return nil, fmt.Errorf("VM '%s' already exists", dstName)
	}

	progress := func(phase, msg string) {
		if progressFn != nil {
			progressFn(phase, msg)
		}
	}

	// Pause source VM if running (freezes vCPUs for consistent disk copy).
	// Only Firecracker supports pause via API. QEMU VMs must be stopped first.
	wasRunning := IsRunning(srcDir)
	if wasRunning {
		if srcSt.Type == "qemu" {
			return nil, fmt.Errorf("QEMU VMs must be stopped before copying (use: ssh boxcutter stop %s)", srcName)
		}
		progress("copy", "Pausing source VM...")
		if err := fcPause(srcDir); err != nil {
			return nil, fmt.Errorf("pausing source VM: %w", err)
		}
	}

	// Copy disk: file-based rootfs or legacy COW
	fileRootfs := IsFileRootfs(srcDir)
	progress("copy", "Copying disk image...")
	os.MkdirAll(dstDir, 0755)

	if fileRootfs {
		// File-based rootfs: copy the standalone ext4 directly
		srcRootfs := filepath.Join(srcDir, "rootfs.ext4")
		dstRootfs := filepath.Join(dstDir, "rootfs.ext4")
		if err := copyFile(srcRootfs, dstRootfs); err != nil {
			if wasRunning {
				fcResume(srcDir)
			}
			os.RemoveAll(dstDir)
			return nil, fmt.Errorf("copying rootfs: %w", err)
		}
	} else {
		// Legacy dm-snapshot: copy the COW overlay
		srcCowPath := filepath.Join(srcDir, "cow.img")
		if _, err := os.Stat(srcCowPath); err != nil {
			if wasRunning {
				fcResume(srcDir)
			}
			return nil, fmt.Errorf("source COW image not found")
		}
		dstCowPath := filepath.Join(dstDir, "cow.img")
		if err := copyFile(srcCowPath, dstCowPath); err != nil {
			if wasRunning {
				fcResume(srcDir)
			}
			os.RemoveAll(dstDir)
			return nil, fmt.Errorf("copying COW: %w", err)
		}
	}

	// Resume source VM immediately after copy
	if wasRunning {
		progress("copy", "Resuming source VM...")
		if err := fcResume(srcDir); err != nil {
			log.Printf("Warning: failed to resume source VM %s: %v", srcName, err)
		}
	}

	// Set up destination VM state with same golden version but new identity
	goldenPath := GoldenPathForVersion(m.GoldenDir(), srcSt.GoldenVer)
	if _, err := os.Stat(goldenPath); err != nil {
		// Try current golden
		goldenPath = m.cfg.Storage.GoldenLocalPath
	}

	existingMarks := m.collectExistingMarks()
	mark := AllocateMark(dstName, existingMarks)
	tap := TAPName(dstName)

	// For legacy COW VMs, create a file-based rootfs from golden for the destination.
	// For file-based VMs, the rootfs was already copied above.
	if !fileRootfs {
		progress("copy", "Creating disk...")
		if err := CreateRootfs(dstDir, goldenPath, srcSt.Disk); err != nil {
			os.RemoveAll(dstDir)
			return nil, fmt.Errorf("creating rootfs: %w", err)
		}
	}

	dstSt := &VMState{
		Name:        dstName,
		Type:        srcSt.Type,
		Description: srcSt.Description,
		VCPU:        srcSt.VCPU,
		RAMMIB:      srcSt.RAMMIB,
		Mark:        mark,
		Mode:        srcSt.Mode,
		MAC:         fixedMAC,
		Disk:        srcSt.Disk,
		TAP:         tap,
		Created:     time.Now().Format(time.RFC3339),
		GoldenVer:   srcSt.GoldenVer,
	}
	if err := SaveVMState(dstDir, dstSt); err != nil {
		os.RemoveAll(dstDir)
		return nil, err
	}

	if dstSt.Type != "qemu" {
		if err := writeFirecrackerConfig(dstDir, dstSt); err != nil {
			os.RemoveAll(dstDir)
			return nil, err
		}
	}

	// Mount the copied rootfs and update hostname + wipe Tailscale state
	mountDir, err := os.MkdirTemp("", "bc-mount-")
	if err == nil {
		if run("mount", RootfsPath(dstDir), mountDir) == nil {
			os.WriteFile(filepath.Join(mountDir, "etc/hostname"), []byte(dstName+"\n"), 0644)
			os.RemoveAll(filepath.Join(mountDir, "var/lib/tailscale"))
			// Also remove SSH host keys so the copy gets fresh ones
			exec.Command("rm", "-f",
				filepath.Join(mountDir, "etc/ssh/ssh_host_rsa_key"),
				filepath.Join(mountDir, "etc/ssh/ssh_host_rsa_key.pub"),
				filepath.Join(mountDir, "etc/ssh/ssh_host_ecdsa_key"),
				filepath.Join(mountDir, "etc/ssh/ssh_host_ecdsa_key.pub"),
				filepath.Join(mountDir, "etc/ssh/ssh_host_ed25519_key"),
				filepath.Join(mountDir, "etc/ssh/ssh_host_ed25519_key.pub"),
			).Run()
			log.Printf("CopyVM %s: hostname set, tailscale state wiped, SSH keys regenerated", dstName)
			run("umount", mountDir)
		} else {
			log.Printf("CopyVM %s: WARNING — could not mount rootfs to update hostname/tailscale", dstName)
		}
		os.RemoveAll(mountDir)
	}

	// Start the new VM and run post-start (Tailscale join, vmid registration)
	resp, err := m.startVM(dstSt, progress)
	if err != nil {
		CleanupSnapshot(dstDir)
		os.RemoveAll(dstDir)
		return nil, fmt.Errorf("starting copied VM: %w", err)
	}

	m.postStartVM(dstSt, resp, progress)

	return resp, nil
}

// copyFile copies a file using cp --sparse=always.
func copyFile(src, dst string) error {
	return run("cp", "--sparse=always", src, dst)
}

// MigrateVM migrates a VM to another node using Firecracker snapshots.
// The VM is paused (not stopped), its state is snapshotted, and it resumes
// on the target node with all processes and memory intact.
//
// Flow:
//   Phase 1 (VM running):  Pre-stage golden image + create target dirs
//   Phase 2 (VM paused):   Snapshot → transfer → resume on target → verify healthy
//   Phase 3 (verified):    Stop source, cleanup (or rollback: resume source)
//
// Safety invariant: source VM stays PAUSED (not stopped) until target is confirmed
// healthy. If anything fails, source is resumed and migration is rolled back.
func (m *Manager) MigrateVM(name, targetAddr, targetBridgeIP string) (*MigrateResponse, error) {
	vmDir := VMDir(name)
	st, err := LoadVMState(vmDir)
	if err != nil {
		return nil, fmt.Errorf("VM '%s' not found", name)
	}

	// QEMU VMs don't support snapshot-based live migration (yet).
	// Stopped QEMU VMs can still be relocated via file transfer.
	if st.Type == "qemu" && IsRunning(vmDir) {
		return nil, fmt.Errorf("live migration is not supported for QEMU VMs (stop the VM first)")
	}

	clusterKey := "/etc/boxcutter/secrets/cluster-ssh.key"
	dstVMDir := fmt.Sprintf("/var/lib/boxcutter/vms/%s/", name)

	// --- Stopped VM: just transfer files, no snapshot needed ---
	if !IsRunning(vmDir) {
		return m.relocateStoppedVM(name, st, vmDir, dstVMDir, targetAddr, targetBridgeIP, clusterKey)
	}

	fileRootfs := IsFileRootfs(vmDir)
	var diskName string
	if fileRootfs {
		diskName = "rootfs.ext4"
	} else {
		diskName = "cow.img"
	}

	// Set up SSH ControlMaster — one key exchange shared by all transfers.
	// This eliminates repeated SSH handshakes (saves ~0.5s per connection).
	// ServerAliveInterval/CountMax detect dead connections in ~30s instead of
	// waiting for TCP keepalive (~15min), preventing indefinite hangs during
	// network partitions while the VM is paused.
	controlPath := fmt.Sprintf("/tmp/bc-migrate-%s", name)
	sshBase := []string{"-i", clusterKey, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3", "-o", "ConnectTimeout=10"}
	masterArgs := append([]string{"-fN", "-o", "ControlMaster=yes", "-o", "ControlPath=" + controlPath}, sshBase...)
	masterArgs = append(masterArgs, "ubuntu@"+targetBridgeIP)
	if out, err := exec.Command("ssh", masterArgs...).CombinedOutput(); err != nil {
		log.Printf("Migrating %s: SSH ControlMaster failed (will use direct connections): %s", name, string(out))
		controlPath = "" // fall back to direct connections
	}
	defer func() {
		if controlPath != "" {
			exec.Command("ssh", "-o", "ControlPath="+controlPath, "-O", "exit", "ubuntu@"+targetBridgeIP).Run()
		}
	}()

	// Build SSH options that use the control socket when available
	sshArgs := append([]string{}, sshBase...)
	sshOpts := fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=10 -o ServerAliveCountMax=3 -o ConnectTimeout=10", clusterKey)
	if controlPath != "" {
		sshArgs = append(sshArgs, "-o", "ControlPath="+controlPath)
		sshOpts += " -o ControlPath=" + controlPath
	}

	// cleanTarget removes pre-staged and snapshot files from the target node.
	// Tries the ControlMaster connection first; falls back to direct SSH if dead.
	cleanTarget := func() {
		cleanArgs := []string{"sudo", "rm", "-rf", dstVMDir, fmt.Sprintf("/dev/shm/bc-%s", name)}
		cleanCmd := exec.Command("ssh", append(append([]string{}, sshArgs...), append([]string{"ubuntu@" + targetBridgeIP}, cleanArgs...)...)...)
		if err := cleanCmd.Run(); err != nil && controlPath != "" {
			// ControlMaster may be dead — retry with direct SSH (no ControlPath)
			log.Printf("Migrating %s: cleanup via ControlMaster failed, retrying direct SSH", name)
			directArgs := append(append([]string{}, sshBase...), append([]string{"ubuntu@" + targetBridgeIP}, cleanArgs...)...)
			exec.Command("ssh", directArgs...).Run()
		}
	}

	// --- Phase 1: Pre-stage while VM is still running (zero downtime) ---
	phase1Start := time.Now()
	log.Printf("Migrating %s to %s: pre-staging (VM still running)", name, targetAddr)

	// Pre-flight: check target has enough RAM before doing any work (Bug #102)
	preflightClient := &http.Client{Timeout: 10 * time.Second}
	healthURL := fmt.Sprintf("http://%s/api/health", targetAddr)
	if hResp, err := preflightClient.Get(healthURL); err == nil {
		var health map[string]interface{}
		json.NewDecoder(hResp.Body).Decode(&health)
		hResp.Body.Close()
		if freeMiB, ok := health["ram_free_mib"].(float64); ok {
			if int(freeMiB) < st.RAMMIB {
				return nil, fmt.Errorf("target has %dMiB free but VM needs %dMiB", int(freeMiB), st.RAMMIB)
			}
		}
	}

	// dm-snapshot VMs need the golden image on the target
	if !fileRootfs && st.GoldenVer != "" && st.GoldenVer != "unversioned" {
		if err := m.ensureTargetHasGolden(st.GoldenVer, targetAddr, targetBridgeIP); err != nil {
			return nil, fmt.Errorf("golden transfer: %w", err)
		}
	}

	// Clean stale /dev/shm snapshot files from any previous migration of this VM
	// (Bug #89: partial vm.mem from an interrupted migration was picked up by
	// ImportSnapshot, which checks /dev/shm before vmDir). Also clean stale
	// vm.snap/vm.mem from vmDir to avoid the same issue in reverse.
	prepCmd := exec.Command("ssh", append(sshArgs, "ubuntu@"+targetBridgeIP,
		"sudo", "rm", "-rf", fmt.Sprintf("/dev/shm/bc-%s", name), fmt.Sprintf("/dev/shm/bc-%s-mig", name),
		"&&", "sudo", "mkdir", "-p", dstVMDir,
		"&&", "sudo", "rm", "-f", dstVMDir+"vm.snap", dstVMDir+"vm.mem")...)
	if out, err := prepCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("prep target: %s: %w", string(out), err)
	}

	// Pre-sync disk using tar --sparse. This uses SEEK_DATA/SEEK_HOLE to read
	// only allocated blocks, not the full sparse file. For a 50GB sparse file
	// with 6.5GB actual data, this reads 6.5GB instead of 50GB (~7x faster).
	preSyncStart := time.Now()
	log.Printf("Migrating %s: pre-syncing %s with tar --sparse", name, diskName)
	preSyncCmd := exec.Command("bash", "-c", fmt.Sprintf(
		"tar --sparse -cf - -C %s %s | %s ubuntu@%s 'sudo tar --sparse -xf - -C %s'",
		vmDir, diskName, sshOpts, targetBridgeIP, dstVMDir))
	if out, err := preSyncCmd.CombinedOutput(); err != nil {
		cleanTarget() // clean up mkdir + any partial pre-sync files on target
		return nil, fmt.Errorf("pre-sync disk transfer failed (aborting migration): %s: %w", string(out), err)
	}
	log.Printf("Migrating %s: pre-sync completed in %s (phase 1 total: %s)",
		name, time.Since(preSyncStart).Round(time.Millisecond), time.Since(phase1Start).Round(time.Millisecond))

	// Ensure guest memory pages are faulted in before snapshotting. After snapshot
	// restore, Firecracker lazily faults pages from the mmapped vm.mem file. If the
	// background prefault goroutine (from ImportSnapshot) hasn't completed yet, the
	// snapshot creation will be extremely slow (~32s vs ~260ms for 512MB).
	pidData, _ := os.ReadFile(filepath.Join(vmDir, "firecracker.pid"))
	if len(pidData) > 0 {
		var pid int
		fmt.Sscanf(string(pidData), "%d", &pid)
		if pid > 0 {
			fcPrefaultMemory(pid, name)
		}
	}

	// --- Phase 2: Pause + Snapshot + delta transfer (downtime starts) ---
	downtimeStart := time.Now()
	log.Printf("Migrating %s: pausing VM (downtime starts)", name)
	if err := fcPause(vmDir); err != nil {
		cleanTarget() // clean up pre-synced files on target
		return nil, fmt.Errorf("pause: %w", err)
	}
	log.Printf("Migrating %s: VM paused in %s", name, time.Since(downtimeStart).Round(time.Millisecond))

	// From here on, any error must resume the source VM — but only after
	// verifying the target doesn't have a running copy (prevents split-brain
	// when import succeeds but the HTTP response is lost due to partition).
	rollback := func(reason string) {
		log.Printf("Migrating %s: ROLLBACK — %s", name, reason)

		// Check if target actually has a running copy (split-brain prevention)
		splitBrainCheck := &http.Client{Timeout: 5 * time.Second}
		checkResp, checkErr := splitBrainCheck.Get(fmt.Sprintf("http://%s/api/vms/%s", targetAddr, name))
		if checkErr == nil {
			var detail map[string]interface{}
			json.NewDecoder(checkResp.Body).Decode(&detail)
			checkResp.Body.Close()
			if s, _ := detail["status"].(string); s == "running" {
				// Target has a running copy — do NOT resume source (would cause split-brain).
				// Instead, commit to the target: stop source, clean up.
				log.Printf("Migrating %s: SPLIT-BRAIN PREVENTED — target has running copy, committing to target", name)
				if m.vmid != nil {
					m.vmid.Deregister(name)
				}
				m.stopVM(name)
				CleanupSnapshot(vmDir)
				os.RemoveAll(vmDir)
				os.RemoveAll(filepath.Join("/dev/shm", "bc-"+name+"-mig"))
				os.RemoveAll(filepath.Join("/dev/shm", "bc-"+name))
				return
			}
		}

		log.Printf("Migrating %s: resuming source VM", name)
		if err := fcResume(vmDir); err != nil {
			log.Printf("Migrating %s: WARNING — failed to resume source: %v", name, err)
		}
		cleanTarget()
		os.RemoveAll(filepath.Join("/dev/shm", "bc-"+name+"-mig")) // clean source /dev/shm snapshot files
		// Clean up disk-based snapshot files from vmDir (Bug #106).
		// When fcSnapshot falls back to disk, it writes vm.snap and vm.mem
		// into vmDir. These must be removed after rollback or they leak disk
		// space and interfere with future migrations.
		os.Remove(filepath.Join(vmDir, "vm.snap"))
		os.Remove(filepath.Join(vmDir, "vm.mem"))
	}

	// Note: KVM dirty page tracking setup is slow (~25s for 512MB) on the first
	// snapshot after snapshot restore. A warm-up snapshot was tried (Bug #96) but
	// removed (Bug #100): the warm-up pays the same 25s cost, then the real snapshot
	// is fast (~700ms), making total ~25.7s vs ~25s without warm-up. Net effect for
	// single migrations (the common case) is +0.7s overhead. The warm-up only helps
	// if the same paused session takes multiple snapshots (e.g., failed retry), which
	// is rare because rollback resumes the VM, resetting KVM tracking.

	// Snapshot to regular files (Firecracker requires truncatable mem file, not FIFO)
	snapStart := time.Now()
	snapPath, memPath, err := fcSnapshot(vmDir)
	if err != nil {
		rollback("snapshot failed: " + err.Error())
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	log.Printf("Migrating %s: snapshot created in %s", name, time.Since(snapStart).Round(time.Millisecond))

	memInfo, _ := os.Stat(memPath)
	memSize := int64(0)
	if memInfo != nil {
		memSize = memInfo.Size()
	}

	// Transfer mem + snap after pause. Disk delta is skipped because:
	// 1. Pre-sync already transferred all allocated blocks
	// 2. Blocks written during pre-sync are in the VM's page cache (part of vm.mem)
	// 3. After snapshot/restore, page cache data is correct (comes from memory dump)
	// 4. Skipping the disk delta eliminates the biggest downtime contributor
	//
	// Safety note: if the VM writes >RAM worth of unique blocks during pre-sync,
	// some evicted dirty pages could be missing from page cache. This is extremely
	// unlikely for typical workloads (~seconds of pre-sync, idle or light VM load).
	// If data integrity under heavy I/O is critical, re-enable disk delta transfer.
	xferStart := time.Now()
	log.Printf("Migrating %s: transferring snapshot (mem=%dMB) + snap (disk pre-synced, skipping delta)", name, memSize/1024/1024)

	// Decide where to write snapshot files on target: /dev/shm for speed if space permits,
	// otherwise vmDir (on disk). Firecracker mmaps the mem file, so /dev/shm usage persists
	// until the VM exits. We only reserve 1.2x (import + margin) rather than 2.2x, because
	// fcSnapshot already has its own /dev/shm check and disk fallback for future exports.
	// The 2.2x formula was too conservative: 4GB VMs always fell back to disk on import
	// (3m36s downtime) even when /dev/shm had enough space for the import itself (Bug #92).
	dstSnapDir := dstVMDir
	dstShmDir := fmt.Sprintf("/dev/shm/bc-%s", name)
	shmCheckCmd := exec.Command("ssh", append(sshArgs, "ubuntu@"+targetBridgeIP,
		"df", "--output=avail", "-B1", "/dev/shm")...)
	if shmOut, err := shmCheckCmd.Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(shmOut)), "\n")
		if len(lines) >= 2 {
			avail, _ := strconv.ParseInt(strings.TrimSpace(lines[len(lines)-1]), 10, 64)
			needed := memSize + memSize/5 // 1x mem + 20% margin (fcSnapshot handles export fallback separately)
			if avail > needed {
				// Use /dev/shm on target for fast writes
				mkShmCmd := exec.Command("ssh", append(sshArgs, "ubuntu@"+targetBridgeIP,
					"sudo", "mkdir", "-p", dstShmDir)...)
				mkShmCmd.Run()
				dstSnapDir = dstShmDir
				log.Printf("Migrating %s: target /dev/shm has %dMB free (need %dMB), using tmpfs",
					name, avail/1024/1024, needed/1024/1024)
			} else {
				log.Printf("Migrating %s: target /dev/shm has %dMB free (need %dMB), using disk",
					name, avail/1024/1024, needed/1024/1024)
			}
		}
	}

	// Send snap first (tiny, <1s), then mem (large). Sequential avoids SSH ControlMaster
	// head-of-line blocking that occurs when parallel transfers share a multiplexed connection.

	// vm.snap first (tiny, a few KB)
	snapXferStart := time.Now()
	snapCmd := exec.Command("bash", "-c", fmt.Sprintf(
		"cat %s | %s ubuntu@%s 'sudo tee %s/vm.snap > /dev/null'",
		snapPath, sshOpts, targetBridgeIP, dstSnapDir))
	if out, err := snapCmd.CombinedOutput(); err != nil {
		rollback("snap transfer failed: " + string(out) + ": " + err.Error())
		return nil, fmt.Errorf("snap transfer: %w", err)
	}
	log.Printf("Migrating %s: snap transfer completed in %s", name, time.Since(snapXferStart).Round(time.Millisecond))

	// Memory file — use dd with 4M blocks for throughput
	memStart := time.Now()
	memCmd := exec.Command("bash", "-c", fmt.Sprintf(
		"dd if=%s bs=4M 2>/dev/null | %s ubuntu@%s 'sudo dd of=%s/vm.mem bs=4M'",
		memPath, sshOpts, targetBridgeIP, dstSnapDir))
	if out, err := memCmd.CombinedOutput(); err != nil {
		rollback("mem transfer failed: " + string(out) + ": " + err.Error())
		return nil, fmt.Errorf("mem transfer: %w", err)
	}
	log.Printf("Migrating %s: mem transfer completed in %s", name, time.Since(memStart).Round(time.Millisecond))
	log.Printf("Migrating %s: all transfers completed in %s", name, time.Since(xferStart).Round(time.Millisecond))

	// --- Resume on target (import-snapshot) ---
	importStart := time.Now()
	log.Printf("Migrating %s: resuming on target %s", name, targetAddr)
	stJSON, _ := json.Marshal(st)
	targetClient := &http.Client{Timeout: 2 * time.Minute}
	importURL := fmt.Sprintf("http://%s/api/vms/%s/import-snapshot", targetAddr, name)
	resp, err := targetClient.Post(importURL, "application/json", bytes.NewReader(stJSON))
	if err != nil {
		rollback("import-snapshot request failed: " + err.Error())
		return nil, fmt.Errorf("import-snapshot request: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode >= 300 {
		rollback("import-snapshot returned " + fmt.Sprint(resp.StatusCode))
		return nil, fmt.Errorf("import-snapshot failed: %s", string(body))
	}

	var importResp CreateResponse
	json.Unmarshal(body, &importResp)
	log.Printf("Migrating %s: import-snapshot completed in %s", name, time.Since(importStart).Round(time.Millisecond))

	// --- Phase 3: Verify target is healthy before committing ---
	verifyStart := time.Now()
	log.Printf("Migrating %s: verifying target is healthy...", name)
	targetHealthy := false
	for i := 0; i < 30; i++ { // up to ~60 seconds (2s intervals × 30)
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		checkResp, err := targetClient.Get(fmt.Sprintf("http://%s/api/vms/%s", targetAddr, name))
		if err != nil {
			continue
		}
		var detail map[string]interface{}
		json.NewDecoder(checkResp.Body).Decode(&detail)
		checkResp.Body.Close()
		if s, _ := detail["status"].(string); s == "running" {
			targetHealthy = true
			break
		}
	}

	if !targetHealthy {
		rollback("target VM not healthy after 60s")
		destroyReq, _ := http.NewRequest("DELETE", fmt.Sprintf("http://%s/api/vms/%s", targetAddr, name), nil)
		targetClient.Do(destroyReq)
		return nil, fmt.Errorf("target VM not healthy — rolled back to source")
	}
	log.Printf("Migrating %s: target verified healthy in %s", name, time.Since(verifyStart).Round(time.Millisecond))

	// --- Target confirmed healthy — commit: stop source, cleanup ---
	downtime := time.Since(downtimeStart)
	log.Printf("Migration complete: %s → %s | mem=%dMB | downtime=%s | phase1=%s | transfers=%s | verify=%s",
		name, targetAddr, memSize/1024/1024,
		downtime.Round(time.Millisecond),
		time.Since(phase1Start).Round(time.Millisecond),
		time.Since(xferStart).Round(time.Millisecond),
		time.Since(verifyStart).Round(time.Millisecond))

	if m.vmid != nil {
		m.vmid.Deregister(name)
	}
	m.stopVM(name)
	CleanupSnapshot(vmDir) // release loop devices / dm-snapshot before removing files
	if err := os.RemoveAll(vmDir); err != nil {
		log.Printf("Migrating %s: WARNING — failed to remove vmDir: %v", name, err)
	}
	if err := os.RemoveAll(filepath.Join("/dev/shm", "bc-"+name+"-mig")); err != nil {
		log.Printf("Migrating %s: WARNING — failed to remove /dev/shm export dir: %v", name, err)
	}
	if err := os.RemoveAll(filepath.Join("/dev/shm", "bc-"+name)); err != nil {
		log.Printf("Migrating %s: WARNING — failed to remove /dev/shm import dir: %v", name, err)
	}

	return &MigrateResponse{
		Name:        importResp.Name,
		TailscaleIP: importResp.TailscaleIP,
		Mark:        importResp.Mark,
		TargetNode:  targetAddr,
		Status:      "migrated",
	}, nil
}

// relocateStoppedVM transfers a stopped VM's files to the target node.
// No snapshot needed — just rsync vm.json + disk image + ensure golden image.
func (m *Manager) relocateStoppedVM(name string, st *VMState, vmDir, dstVMDir, targetAddr, targetBridgeIP, clusterKey string) (*MigrateResponse, error) {
	log.Printf("Relocating stopped VM %s to %s", name, targetAddr)
	fileRootfs := IsFileRootfs(vmDir)

	sshBase := []string{"-i", clusterKey, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3"}
	sshOpts := fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o ServerAliveInterval=10 -o ServerAliveCountMax=3", clusterKey)

	cleanTarget := func() {
		exec.Command("ssh", append(append([]string{}, sshBase...), "ubuntu@"+targetBridgeIP,
			"sudo", "rm", "-rf", dstVMDir)...).Run()
	}

	// dm-snapshot VMs need the golden image on the target
	if !fileRootfs && st.GoldenVer != "" && st.GoldenVer != "unversioned" {
		if err := m.ensureTargetHasGolden(st.GoldenVer, targetAddr, targetBridgeIP); err != nil {
			return nil, fmt.Errorf("golden transfer: %w", err)
		}
	}

	// Create target directory
	mkdirCmd := exec.Command("ssh", append(append([]string{}, sshBase...), "ubuntu@"+targetBridgeIP,
		"sudo", "mkdir", "-p", dstVMDir)...)
	if out, err := mkdirCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("mkdir on target: %s: %w", string(out), err)
	}

	// Transfer all VM files using tar --sparse (reads only allocated blocks,
	// not the full sparse extent — 7x faster for typical sparse rootfs files)
	xferStart := time.Now()
	var diskName string
	if fileRootfs {
		diskName = "rootfs.ext4"
	} else {
		diskName = "cow.img"
	}
	tarCmd := exec.Command("bash", "-c", fmt.Sprintf(
		"tar --sparse -cf - -C %s %s vm.json | %s ubuntu@%s 'sudo tar --sparse -xf - -C %s'",
		vmDir, diskName, sshOpts, targetBridgeIP, dstVMDir))
	if out, err := tarCmd.CombinedOutput(); err != nil {
		cleanTarget()
		return nil, fmt.Errorf("tar transfer %s: %s: %w", diskName, string(out), err)
	}

	// Transfer snapshot.json if it exists (dm-snapshot VMs only)
	if !fileRootfs {
		snapJSONPath := filepath.Join(vmDir, "snapshot.json")
		if _, err := os.Stat(snapJSONPath); err == nil {
			catCmd := exec.Command("bash", "-c", fmt.Sprintf(
				"cat %s | %s ubuntu@%s 'sudo tee %ssnapshot.json > /dev/null'",
				snapJSONPath, sshOpts, targetBridgeIP, dstVMDir))
			catCmd.CombinedOutput() // best effort
		}
	}
	log.Printf("Relocated stopped VM %s: file transfer took %s", name, time.Since(xferStart).Round(time.Millisecond))

	// Verify target has the VM files before deleting source
	verifyCmd := exec.Command("ssh", append(append([]string{}, sshBase...), "ubuntu@"+targetBridgeIP,
		"test", "-f", dstVMDir+"vm.json", "-a", "-f", dstVMDir+diskName)...)
	if err := verifyCmd.Run(); err != nil {
		cleanTarget()
		return nil, fmt.Errorf("target verification failed — source preserved: %w", err)
	}

	// Guard: if VM was started concurrently, abort (don't delete a running VM's files)
	if IsRunning(vmDir) {
		cleanTarget()
		return nil, fmt.Errorf("VM '%s' was started during relocation — aborting", name)
	}

	// Clean up source
	if m.vmid != nil {
		m.vmid.Deregister(name)
	}
	CleanupSnapshot(vmDir) // clean up dm-snapshot loop devices before removing files
	os.RemoveAll(vmDir)
	log.Printf("Relocated stopped VM %s to %s", name, targetAddr)

	return &MigrateResponse{
		Name:       name,
		TargetNode: targetAddr,
		Status:     "relocated",
	}, nil
}

// ImportSnapshot loads a VM from a Firecracker snapshot on this node.
// The COW, vm.snap, and vm.mem files must already be in the VM directory.
// This resumes the VM exactly where it was paused — no reboot.
func (m *Manager) ImportSnapshot(st *VMState) (*CreateResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vmDir := VMDir(st.Name)
	shmDir := filepath.Join("/dev/shm", "bc-"+st.Name)

	// Check capacity: reject if this VM would overcommit the node (Bug #102)
	sysRAM := m.getSystemRAMMiB()
	if sysRAM > 0 {
		allocatedRAM := m.getAllocatedRAMMiB()
		if allocatedRAM+st.RAMMIB > sysRAM*90/100 {
			return nil, &CapacityError{msg: "node is full (import rejected)"}
		}
	}

	// Check if a VM with this name already exists (running or stopped)
	if _, err := LoadVMState(vmDir); err == nil {
		if IsRunning(vmDir) {
			return nil, fmt.Errorf("VM '%s' already exists and is running", st.Name)
		}
		return nil, fmt.Errorf("VM '%s' already exists (stopped)", st.Name)
	}

	os.MkdirAll(vmDir, 0755)

	// Reallocate mark/TAP on this node
	existingMarks := m.collectExistingMarks()
	st.Mark = AllocateMark(st.Name, existingMarks)
	st.TAP = TAPName(st.Name)

	if err := SaveVMState(vmDir, st); err != nil {
		os.RemoveAll(shmDir)
		os.RemoveAll(vmDir)
		return nil, err
	}

	// Set up dm-snapshot from golden + COW (same block device path as source)
	goldenPath := m.goldenPathForVM(st)
	if err := EnsureSnapshot(vmDir, goldenPath); err != nil {
		os.RemoveAll(shmDir)
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("ensuring snapshot: %w", err)
	}

	// Clean stale TAP/rules (idempotent) then set up fresh
	TeardownTAP(st.TAP, st.Mark)
	if err := SetupTAP(st.TAP, st.Mark); err != nil {
		CleanupSnapshot(vmDir)
		os.RemoveAll(shmDir)
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("setting up TAP: %w", err)
	}

	if st.Mode == "paranoid" {
		if err := SetupParanoidMode(st.TAP); err != nil {
			TeardownTAP(st.TAP, st.Mark)
			CleanupSnapshot(vmDir)
			os.RemoveAll(shmDir)
			os.RemoveAll(vmDir)
			return nil, fmt.Errorf("paranoid mode: %w", err)
		}
	}

	// Clean stale sockets
	os.Remove(filepath.Join(vmDir, "api.sock"))
	os.Remove(filepath.Join(vmDir, "vsock.sock"))

	// Launch Firecracker with just an API socket (no config — snapshot provides everything)
	logFile, _ := os.Create(filepath.Join(vmDir, "firecracker.log"))
	cmd := exec.Command("firecracker", "--api-sock", filepath.Join(vmDir, "api.sock"))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		TeardownTAP(st.TAP, st.Mark)
		CleanupSnapshot(vmDir)
		os.RemoveAll(shmDir)
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("starting firecracker: %w", err)
	}
	logFile.Close()

	os.WriteFile(filepath.Join(vmDir, "firecracker.pid"),
		[]byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644)
	go cmd.Wait()

	// Wait for API socket to be ready by actually connecting (not just file existence).
	// os.Stat alone can return true before Firecracker is accepting connections, causing
	// the subsequent /snapshot/load PUT to timeout with "dial unix ... i/o timeout".
	sockPath := filepath.Join(vmDir, "api.sock")
	sockReady := false
	for i := 0; i < 50; i++ {
		conn, dialErr := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			sockReady = true
			break
		}
		// Check if FC crashed (process exited before socket appeared)
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			fcLog, _ := os.ReadFile(filepath.Join(vmDir, "firecracker.log"))
			TeardownTAP(st.TAP, st.Mark)
			CleanupSnapshot(vmDir)
			os.RemoveAll(shmDir)
			os.RemoveAll(vmDir)
			return nil, fmt.Errorf("firecracker crashed on startup: %s", string(fcLog))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !sockReady {
		cmd.Process.Kill()
		TeardownTAP(st.TAP, st.Mark)
		CleanupSnapshot(vmDir)
		os.RemoveAll(shmDir)
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("firecracker API socket not ready after 5s")
	}

	// Load the snapshot. Check /dev/shm first (migration writes there when space permits),
	// fall back to vmDir. Firecracker mmaps the mem file for the process lifetime.
	snapPath := filepath.Join(shmDir, "vm.snap")
	memPath := filepath.Join(shmDir, "vm.mem")
	if _, err := os.Stat(memPath); err != nil {
		// Fall back to vmDir
		snapPath = filepath.Join(vmDir, "vm.snap")
		memPath = filepath.Join(vmDir, "vm.mem")
	}

	// Verify snapshot files exist before calling Firecracker API
	snapStat, snapErr := os.Stat(snapPath)
	memStat, memErr := os.Stat(memPath)
	if snapErr != nil || memErr != nil {
		cmd.Process.Kill()
		os.Remove(filepath.Join(vmDir, "firecracker.pid"))
		TeardownTAP(st.TAP, st.Mark)
		CleanupSnapshot(vmDir)
		os.RemoveAll(shmDir)
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("snapshot files missing: snap=%s (err=%v), mem=%s (err=%v)",
			snapPath, snapErr, memPath, memErr)
	}
	log.Printf("ImportSnapshot %s: loading snap=%s (%d bytes), mem=%s (%d bytes)",
		st.Name, snapPath, snapStat.Size(), memPath, memStat.Size())

	loadBody := map[string]interface{}{
		"snapshot_path": snapPath,
		"mem_backend": map[string]string{
			"backend_type": "File",
			"backend_path": memPath,
		},
		"enable_diff_snapshots": false,
		"resume_vm":             true,
	}
	if err := fcPut(vmDir, "/snapshot/load", loadBody); err != nil {
		// Log Firecracker output before cleanup
		if fcLog, readErr := os.ReadFile(filepath.Join(vmDir, "firecracker.log")); readErr == nil && len(fcLog) > 0 {
			log.Printf("ImportSnapshot %s: Firecracker log:\n%s", st.Name, string(fcLog))
		}
		// Kill Firecracker on failure
		cmd.Process.Kill()
		os.Remove(filepath.Join(vmDir, "firecracker.pid"))
		TeardownTAP(st.TAP, st.Mark)
		CleanupSnapshot(vmDir)
		os.RemoveAll(shmDir)
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("loading snapshot: %w", err)
	}
	// vm.snap can be removed (small, read once). vm.mem stays — Firecracker has it mmapped.
	// If on /dev/shm, this is intentional: tmpfs-backed memory is fine for the VM's lifetime.
	os.Remove(snapPath)
	// If snapshot files were in vmDir, clean up the mem file too (Firecracker has it mmapped,
	// but the disk space is freed only on unlink — we want that for disk-backed files).
	if strings.HasPrefix(memPath, vmDir) {
		os.Remove(memPath)
	}

	log.Printf("VM %s resumed from snapshot (PID %d, mark %d)", st.Name, cmd.Process.Pid, st.Mark)

	// Register with vmid
	if m.vmid != nil {
		vmType := st.Type
		if vmType == "" {
			vmType = "firecracker"
		}
		m.vmid.Register(&vmid.RegisterRequest{
			VMID:        st.Name,
			VMType:      vmType,
			IP:          "10.0.0.2",
			Mark:        st.Mark,
			Mode:        st.Mode,
			GitHubRepo:  st.GitHubRepo,
			GitHubRepos: st.AllGitHubRepos(),
		})
	}

	// Pre-fault guest memory pages so the next snapshot creation is fast (~260ms
	// instead of ~25s). Must run after snapshot load while Firecracker has the mmap.
	go fcPrefaultMemory(cmd.Process.Pid, st.Name)

	// Nudge Tailscale to re-establish its network path through the new node.
	// Uses vsock (host→guest channel) — no SSH/network dependency needed.
	go func() {
		time.Sleep(500 * time.Millisecond)
		for i := 0; i < 5; i++ {
			if err := fcVsockNudge(vmDir, 52); err == nil {
				log.Printf("VM %s: vsock nudge sent for network path update", st.Name)
				return
			}
			time.Sleep(time.Second)
		}
		log.Printf("Warning: vsock nudge failed for %s after migration", st.Name)
	}()

	return &CreateResponse{
		Name:        st.Name,
		TailscaleIP: st.TailscaleIP,
		Mark:        st.Mark,
		Mode:        st.Mode,
		Status:      "running",
	}, nil
}

// ensureTargetHasGolden checks if the target node has the required golden image
// version, and rsyncs it over if not.
func (m *Manager) ensureTargetHasGolden(version, targetAddr, targetBridgeIP string) error {
	// Check if target has this version
	checkURL := fmt.Sprintf("http://%s/api/golden/%s", targetAddr, version)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(checkURL)
	if err != nil {
		return fmt.Errorf("checking target golden: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Printf("Target node already has golden %s", version)
		return nil
	}

	// Transfer golden image to target
	goldenPath := GoldenPathForVersion(m.GoldenDir(), version)
	if _, err := os.Stat(goldenPath); err != nil {
		return fmt.Errorf("local golden image not found: %s", goldenPath)
	}

	log.Printf("Transferring golden image %s to %s", version, targetBridgeIP)

	clusterKey := "/etc/boxcutter/secrets/cluster-ssh.key"
	sshOpts := fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o ServerAliveInterval=10 -o ServerAliveCountMax=3", clusterKey)
	goldenDir := "/var/lib/boxcutter/golden/"

	// Ensure target golden dir exists
	mkdirCmd := exec.Command("ssh",
		"-i", clusterKey, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3",
		"ubuntu@"+targetBridgeIP, "sudo", "mkdir", "-p", goldenDir)
	if out, err := mkdirCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkdir golden on target: %s: %w", string(out), err)
	}

	rsyncDest := fmt.Sprintf("ubuntu@%s:%s%s.ext4", targetBridgeIP, goldenDir, version)
	rsyncCmd := exec.Command("rsync", "--sparse", "--whole-file", "--compress", "--rsync-path", "sudo rsync",
		"-e", sshOpts, goldenPath, rsyncDest)
	if out, err := rsyncCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync golden: %s: %w", string(out), err)
	}

	log.Printf("Golden image %s transferred to %s", version, targetBridgeIP)
	return nil
}

// MigrateResponse is the result of a migration.
type MigrateResponse struct {
	Name        string `json:"name"`
	TailscaleIP string `json:"tailscale_ip,omitempty"`
	Mark        int    `json:"mark"`
	TargetNode  string `json:"target_node"`
	Status      string `json:"status"`
}

// ImportVM receives a VM state and starts it. The COW image must already
// be at the expected path.
func (m *Manager) ImportVM(st *VMState) (*CreateResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vmDir := VMDir(st.Name)
	os.MkdirAll(vmDir, 0755)

	// Reallocate mark on this node
	existingMarks := m.collectExistingMarks()
	st.Mark = AllocateMark(st.Name, existingMarks)
	st.TAP = TAPName(st.Name)

	if err := SaveVMState(vmDir, st); err != nil {
		return nil, err
	}

	// Ensure snapshot is active (use VM's golden version)
	goldenPath := m.goldenPathForVM(st)
	if err := EnsureSnapshot(vmDir, goldenPath); err != nil {
		return nil, err
	}

	// Write Firecracker config
	if err := writeFirecrackerConfig(vmDir, st); err != nil {
		return nil, err
	}

	return m.startVM(st, nil)
}

// GetActivity returns a VM's latest tapegun activity report.
func (m *Manager) GetActivity(name string) (*vmid.ActivityReport, error) {
	if m.vmid == nil {
		return nil, fmt.Errorf("vmid client not configured")
	}
	vmDir := VMDir(name)
	if _, err := LoadVMState(vmDir); err != nil {
		return nil, fmt.Errorf("VM '%s' not found", name)
	}
	return m.vmid.GetVMActivity(name)
}

// SendMessage sends a tapegun message to a VM.
func (m *Manager) SendMessage(name string, msg *vmid.Message) error {
	if m.vmid == nil {
		return fmt.Errorf("vmid client not configured")
	}
	vmDir := VMDir(name)
	if _, err := LoadVMState(vmDir); err != nil {
		return fmt.Errorf("VM '%s' not found", name)
	}
	return m.vmid.PostMessage(name, msg)
}

// AllActivity returns tapegun activity for all VMs on this node.
func (m *Manager) AllActivity() ([]vmid.VMActivitySummary, error) {
	if m.vmid == nil {
		return nil, fmt.Errorf("vmid client not configured")
	}
	return m.vmid.GetAllActivity()
}
