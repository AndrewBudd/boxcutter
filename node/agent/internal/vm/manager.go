package vm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
	mu     sync.Mutex
	cfg    *config.Config
	vmid   *vmid.Client
}

func NewManager(cfg *config.Config, vmidClient *vmid.Client) *Manager {
	return &Manager{cfg: cfg, vmid: vmidClient}
}

// ProgressFunc is called with phase updates during VM creation.
type ProgressFunc func(phase, message string)

// CreateRequest is the API input for creating a VM.
type CreateRequest struct {
	Name           string   `json:"name"`
	VCPU           int      `json:"vcpu,omitempty"`
	RAMMIB         int      `json:"ram_mib,omitempty"`
	Disk           string   `json:"disk,omitempty"`
	CloneURL       string   `json:"clone_url,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	AuthorizedKeys []string `json:"authorized_keys,omitempty"`

	progressFn ProgressFunc `json:"-"`
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
	m.mu.Lock()
	defer m.mu.Unlock()

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

	// Check capacity: reject if adding this VM would exceed 90% of system RAM
	sysRAM := m.getSystemRAMMiB()
	if sysRAM > 0 {
		allocatedRAM := m.getAllocatedRAMMiB()
		if allocatedRAM+req.RAMMIB > sysRAM*90/100 {
			return nil, &CapacityError{msg: "node is full"}
		}
	}

	vmDir := VMDir(req.Name)
	if _, err := os.Stat(vmDir); err == nil {
		return nil, fmt.Errorf("VM '%s' already exists", req.Name)
	}

	goldenPath := m.cfg.Storage.GoldenLocalPath
	if _, err := os.Stat(goldenPath); err != nil {
		return nil, fmt.Errorf("golden image not found at %s", goldenPath)
	}

	// Resolve golden version
	goldenVer := resolveGoldenVersion(goldenPath)

	os.MkdirAll(vmDir, 0755)

	// Allocate mark
	existingMarks := m.collectExistingMarks()
	mark := AllocateMark(req.Name, existingMarks)
	tap := TAPName(req.Name)

	// Parse clone URL to owner/repo
	githubRepo := ""
	if req.CloneURL != "" {
		githubRepo = parseRepoURL(req.CloneURL)
	}

	// Create COW snapshot
	req.progress("snapshot", "Creating disk snapshot...")
	ss, err := CreateSnapshot(vmDir, goldenPath, req.Disk)
	if err != nil {
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("creating snapshot: %w", err)
	}
	SaveSnapshotState(vmDir, ss)

	// Save VM state
	st := &VMState{
		Name:       req.Name,
		VCPU:       req.VCPU,
		RAMMIB:     req.RAMMIB,
		Mark:       mark,
		Mode:       req.Mode,
		MAC:        fixedMAC,
		Disk:       req.Disk,
		TAP:        tap,
		Created:    time.Now().Format(time.RFC3339),
		CloneURL:   req.CloneURL,
		GitHubRepo: githubRepo,
		GoldenVer:  goldenVer,
	}
	if err := SaveVMState(vmDir, st); err != nil {
		CleanupSnapshot(vmDir)
		os.RemoveAll(vmDir)
		return nil, err
	}

	// Write Firecracker config
	if err := writeFirecrackerConfig(vmDir, st); err != nil {
		CleanupSnapshot(vmDir)
		os.RemoveAll(vmDir)
		return nil, err
	}

	// Inject CA cert + SSH keys in a single mount
	m.prepareRootfs(st, req.AuthorizedKeys)

	// Start the VM
	resp, err := m.startVM(st, req.progress)
	if err != nil {
		CleanupSnapshot(vmDir)
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("starting VM: %w", err)
	}

	return resp, nil
}

// Start starts an existing stopped VM.
func (m *Manager) Start(name string) (*CreateResponse, error) {
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

	return m.startVM(st, nil)
}

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

	emit("starting", "Starting Firecracker VM...")
	// Launch Firecracker
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

	// Save PID
	os.WriteFile(filepath.Join(vmDir, "firecracker.pid"),
		[]byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644)

	// Don't wait for firecracker — it runs until stopped
	go cmd.Wait()

	log.Printf("VM %s started (PID %d, mark %d)", st.Name, cmd.Process.Pid, st.Mark)

	// Skip Tailscale/vmid for internal provision VMs
	if strings.HasPrefix(st.Name, "_") {
		return &CreateResponse{
			Name:   st.Name,
			Mark:   st.Mark,
			Mode:   st.Mode,
			Status: "running",
		}, nil
	}

	// Wait for SSH
	emit("ssh", "Waiting for VM to boot...")
	sshKey := m.cfg.SSH.PrivateKeyPath
	if err := WaitForSSH(st.TAP, sshKey, 30*time.Second); err != nil {
		log.Printf("Warning: SSH not ready for %s: %v", st.Name, err)
	}

	// Join Tailscale
	emit("tailscale", "Joining Tailscale network...")
	tsIP := m.joinTailscale(st)

	// Register with vmid
	if m.vmid != nil {
		m.vmid.Register(&vmid.RegisterRequest{
			VMID:       st.Name,
			IP:         "10.0.0.2",
			Mark:       st.Mark,
			Mode:       st.Mode,
			GitHubRepo: st.GitHubRepo,
		})
	}

	// Paranoid mode: inject proxy env
	if st.Mode == "paranoid" {
		emit("paranoid", "Configuring paranoid mode...")
		m.injectProxyEnv(st)
	}

	// Clone repo if specified
	if st.CloneURL != "" {
		emit("clone", fmt.Sprintf("Cloning %s...", st.CloneURL))
		if err := m.cloneRepo(st); err != nil {
			emit("clone_failed", fmt.Sprintf("Warning: %s", err))
		}
	}

	// Update state with Tailscale IP
	if tsIP != "" {
		st.TailscaleIP = tsIP
		SaveVMState(vmDir, st)
	}

	return &CreateResponse{
		Name:        st.Name,
		TailscaleIP: tsIP,
		Mark:        st.Mark,
		Mode:        st.Mode,
		Status:      "running",
	}, nil
}

// Stop stops a running VM.
func (m *Manager) Stop(name string) error {
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

	pidFile := filepath.Join(vmDir, "firecracker.pid")
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		return nil // Not running
	}

	var pid int
	fmt.Sscanf(string(pidData), "%d", &pid)

	// Graceful shutdown via Firecracker API
	apiSock := filepath.Join(vmDir, "api.sock")
	if _, err := os.Stat(apiSock); err == nil {
		run("curl", "-s", "--unix-socket", apiSock,
			"-X", "PUT", "http://localhost/actions",
			"-H", "Content-Type: application/json",
			"-d", `{"action_type":"SendCtrlAltDel"}`)

		for i := 0; i < 10; i++ {
			p, _ := os.FindProcess(pid)
			if p.Signal(nil) != nil {
				break
			}
			time.Sleep(time.Second)
		}
	}

	// Force kill if still running
	if p, _ := os.FindProcess(pid); p != nil {
		p.Signal(nil) // Check if alive
		p.Kill()
	}

	// Cleanup
	os.Remove(pidFile)
	os.Remove(apiSock)

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
	log.Printf("VM %s destroyed", name)
	return nil
}

// Get returns the state and status of a VM.
func (m *Manager) Get(name string) (*VMState, string, error) {
	vmDir := VMDir(name)
	st, err := LoadVMState(vmDir)
	if err != nil {
		return nil, "", fmt.Errorf("VM '%s' not found", name)
	}
	status := "stopped"
	if IsRunning(vmDir) {
		status = "running"
	}
	return st, status, nil
}

// List returns all VMs with their status.
func (m *Manager) List() ([]map[string]interface{}, error) {
	vms, err := ListVMs()
	if err != nil {
		return nil, err
	}
	var result []map[string]interface{}
	for _, st := range vms {
		status := "stopped"
		if IsRunning(VMDir(st.Name)) {
			status = "running"
		}
		result = append(result, map[string]interface{}{
			"name":         st.Name,
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

// RestartAll restarts all VMs found on disk. Called on node agent startup
// to recover VMs after a node reboot.
func (m *Manager) RestartAll() {
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

	return map[string]interface{}{
		"hostname":          hostname,
		"vcpu_total":        cpuCount,
		"ram_total_mib":     sysRAM,
		"ram_allocated_mib": totalRAM,
		"ram_free_mib":      sysRAM - totalRAM,
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
	dmName := "bc-" + st.Name

	fcConfig := map[string]interface{}{
		"boot-source": map[string]string{
			"kernel_image_path": kernelPath,
			"boot_args":        fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on root=/dev/vda rw init=/sbin/init %s", bootIP),
		},
		"drives": []map[string]interface{}{
			{
				"drive_id":       "rootfs",
				"path_on_host":   "/dev/mapper/" + dmName,
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

func (m *Manager) cloneRepo(st *VMState) error {
	sshKey := m.cfg.SSH.PrivateKeyPath

	// Try to get GitHub token
	var ghToken string
	if m.vmid != nil && st.GitHubRepo != "" {
		tok, err := m.vmid.MintGitHubToken(st.Name)
		if err == nil && tok.Token != "" {
			ghToken = tok.Token
		}
	}

	cloneURL := st.CloneURL
	// Expand shorthand owner/repo to full GitHub URL
	if !strings.Contains(cloneURL, "://") && !strings.HasPrefix(cloneURL, "git@") {
		cloneURL = fmt.Sprintf("https://github.com/%s.git", cloneURL)
	}
	if ghToken != "" && st.GitHubRepo != "" {
		cloneURL = fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", ghToken, st.GitHubRepo)
	}

	out, err := VMSSH(st.TAP, sshKey, fmt.Sprintf("git clone '%s' ~/project 2>&1", cloneURL))
	if err != nil {
		log.Printf("Clone failed for %s: %s", st.Name, out)
		return fmt.Errorf("clone failed: %s", strings.TrimSpace(out))
	}

	if ghToken != "" {
		// Strip token from remote, set up git credential helper
		setupCmd := fmt.Sprintf(`cd ~/project && git remote set-url origin 'https://github.com/%s.git'
mkdir -p ~/.config/gh
cat > ~/.config/gh/hosts.yml <<GHEOF
github.com:
    oauth_token: %s
    user: x-access-token
    git_protocol: https
GHEOF
git config --global credential.helper '!f() { echo username=x-access-token; echo password=%s; }; f'`,
			st.GitHubRepo, ghToken, ghToken)
		VMSSH(st.TAP, sshKey, setupCmd)
	}

	log.Printf("VM %s: cloned %s", st.Name, st.CloneURL)
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

// MigrateVM migrates a VM to another node using Firecracker snapshots.
// The VM is paused (not stopped), its state is snapshotted, and it resumes
// on the target node with all processes and memory intact.
//
// Flow:
//   Phase 1 (VM running):  Pre-stage golden image + create target dirs
//   Phase 2 (VM paused):   Snapshot → transfer COW + snapshot + mem → load on target
//   Phase 3:               Cleanup source
func (m *Manager) MigrateVM(name, targetAddr, targetBridgeIP string) (*MigrateResponse, error) {
	vmDir := VMDir(name)
	st, err := LoadVMState(vmDir)
	if err != nil {
		return nil, fmt.Errorf("VM '%s' not found", name)
	}

	clusterKey := "/root/.ssh/cluster-ssh.key"
	dstVMDir := fmt.Sprintf("/var/lib/boxcutter/vms/%s/", name)

	// --- Phase 1: Pre-stage while VM is still running ---
	log.Printf("Migrating %s to %s: pre-staging (VM still running)", name, targetAddr)

	// 1a. Transfer golden image if needed (this is the slow part)
	if st.GoldenVer != "" && st.GoldenVer != "unversioned" {
		if err := m.ensureTargetHasGolden(st.GoldenVer, targetAddr, targetBridgeIP); err != nil {
			return nil, fmt.Errorf("golden transfer: %w", err)
		}
	}

	// 1b. Create destination directory on target
	mkdirCmd := exec.Command("ssh",
		"-i", clusterKey, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"ubuntu@"+targetBridgeIP, "sudo", "mkdir", "-p", dstVMDir)
	if out, err := mkdirCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("mkdir on target: %s: %w", string(out), err)
	}

	// --- Phase 2: Pause + Snapshot + Transfer (downtime starts) ---
	downtimeStart := time.Now()

	if !IsRunning(vmDir) {
		return nil, fmt.Errorf("VM '%s' is not running — cannot snapshot", name)
	}

	// 2a. Pause the VM (freezes vCPUs instantly, sub-millisecond)
	log.Printf("Migrating %s: pausing VM (downtime starts)", name)
	if err := fcPause(vmDir); err != nil {
		return nil, fmt.Errorf("pause: %w", err)
	}

	// 2b. Create Firecracker snapshot (writes vm.snap + vm.mem)
	_, memPath, err := fcSnapshot(vmDir)
	if err != nil {
		// Resume VM on failure so we don't leave it frozen
		fcResume(vmDir)
		return nil, fmt.Errorf("snapshot: %w", err)
	}

	memInfo, _ := os.Stat(memPath)
	memSize := int64(0)
	if memInfo != nil {
		memSize = memInfo.Size()
	}

	// 2c. Stream COW + snapshot + mem to target using tar --sparse (uncompressed)
	// On a local bridge, raw throughput beats CPU-bound compression
	log.Printf("Migrating %s: transferring snapshot (snap + mem + cow)", name)
	tarCmd := fmt.Sprintf(
		"sudo tar --sparse -cf - -C %s cow.img vm.snap vm.mem | ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@%s 'sudo tar --sparse -xf - -C %s'",
		vmDir, clusterKey, targetBridgeIP, dstVMDir)
	streamCmd := exec.Command("bash", "-c", tarCmd)
	if out, err := streamCmd.CombinedOutput(); err != nil {
		// Resume VM on failure
		fcResume(vmDir)
		return nil, fmt.Errorf("transfer: %s: %w", string(out), err)
	}

	// 2d. Stop the source VM now that data is transferred
	// Deregister from vmid first
	if m.vmid != nil {
		m.vmid.Deregister(name)
	}
	m.stopVM(name)

	// 2e. Load snapshot on target node (import-snapshot endpoint)
	stJSON, _ := json.Marshal(st)
	targetClient := &http.Client{Timeout: 5 * time.Minute}
	importURL := fmt.Sprintf("http://%s/api/vms/%s/import-snapshot", targetAddr, name)
	resp, err := targetClient.Post(importURL, "application/json", bytes.NewReader(stJSON))
	if err != nil {
		return nil, fmt.Errorf("import-snapshot request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("import-snapshot failed: %s", string(body))
	}

	var importResp CreateResponse
	json.NewDecoder(resp.Body).Decode(&importResp)

	downtime := time.Since(downtimeStart)
	log.Printf("Migration complete: %s → %s (mem: %d bytes, downtime: %s, IP: %s)",
		name, targetAddr, memSize, downtime.Round(time.Millisecond), importResp.TailscaleIP)

	// --- Phase 3: Cleanup source ---
	os.RemoveAll(vmDir)

	return &MigrateResponse{
		Name:        importResp.Name,
		TailscaleIP: importResp.TailscaleIP,
		Mark:        importResp.Mark,
		TargetNode:  targetAddr,
		Status:      "migrated",
	}, nil
}

// ImportSnapshot loads a VM from a Firecracker snapshot on this node.
// The COW, vm.snap, and vm.mem files must already be in the VM directory.
// This resumes the VM exactly where it was paused — no reboot.
func (m *Manager) ImportSnapshot(st *VMState) (*CreateResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vmDir := VMDir(st.Name)
	os.MkdirAll(vmDir, 0755)

	// Reallocate mark/TAP on this node
	existingMarks := m.collectExistingMarks()
	st.Mark = AllocateMark(st.Name, existingMarks)
	st.TAP = TAPName(st.Name)

	if err := SaveVMState(vmDir, st); err != nil {
		return nil, err
	}

	// Set up dm-snapshot from golden + COW (same block device path as source)
	goldenPath := m.goldenPathForVM(st)
	if err := EnsureSnapshot(vmDir, goldenPath); err != nil {
		return nil, fmt.Errorf("ensuring snapshot: %w", err)
	}

	// Set up TAP + fwmark routing
	if err := SetupTAP(st.TAP, st.Mark); err != nil {
		return nil, fmt.Errorf("setting up TAP: %w", err)
	}

	if st.Mode == "paranoid" {
		if err := SetupParanoidMode(st.TAP); err != nil {
			TeardownTAP(st.TAP, st.Mark)
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
		return nil, fmt.Errorf("starting firecracker: %w", err)
	}
	logFile.Close()

	os.WriteFile(filepath.Join(vmDir, "firecracker.pid"),
		[]byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644)
	go cmd.Wait()

	// Wait for API socket to be ready
	sockPath := filepath.Join(vmDir, "api.sock")
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Load the snapshot — this restores VM state and resumes execution
	snapPath := filepath.Join(vmDir, "vm.snap")
	memPath := filepath.Join(vmDir, "vm.mem")

	loadBody := map[string]interface{}{
		"snapshot_path":        snapPath,
		"mem_backend": map[string]string{
			"backend_type":  "File",
			"backend_path":  memPath,
		},
		"enable_diff_snapshots": false,
		"resume_vm":             true,
	}
	if err := fcPut(vmDir, "/snapshot/load", loadBody); err != nil {
		// Kill Firecracker on failure
		cmd.Process.Kill()
		os.Remove(filepath.Join(vmDir, "firecracker.pid"))
		TeardownTAP(st.TAP, st.Mark)
		return nil, fmt.Errorf("loading snapshot: %w", err)
	}

	log.Printf("VM %s resumed from snapshot (PID %d, mark %d)", st.Name, cmd.Process.Pid, st.Mark)

	// Register with vmid
	if m.vmid != nil {
		m.vmid.Register(&vmid.RegisterRequest{
			VMID:       st.Name,
			IP:         "10.0.0.2",
			Mark:       st.Mark,
			Mode:       st.Mode,
			GitHubRepo: st.GitHubRepo,
		})
	}

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

	// Clean up snapshot files (no longer needed after load)
	os.Remove(snapPath)
	os.Remove(memPath)

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
	resp, err := http.Get(checkURL)
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

	clusterKey := "/root/.ssh/cluster-ssh.key"
	sshOpts := fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null", clusterKey)
	goldenDir := "/var/lib/boxcutter/golden/"

	// Ensure target golden dir exists
	mkdirCmd := exec.Command("ssh",
		"-i", clusterKey, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
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
