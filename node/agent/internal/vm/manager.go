package vm

import (
	"encoding/json"
	"fmt"
	"log"
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

// CreateRequest is the API input for creating a VM.
type CreateRequest struct {
	Name           string   `json:"name"`
	VCPU           int      `json:"vcpu,omitempty"`
	RAMMIB         int      `json:"ram_mib,omitempty"`
	Disk           string   `json:"disk,omitempty"`
	CloneURL       string   `json:"clone_url,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	AuthorizedKeys []string `json:"authorized_keys,omitempty"`
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

	// Inject CA cert into rootfs
	m.injectCACert(st)

	// Inject SSH keys into rootfs
	if len(req.AuthorizedKeys) > 0 {
		// Write orchestrator-provided keys to a temp file
		tmpFile, err := os.CreateTemp("", "bc-authkeys-")
		if err == nil {
			tmpFile.WriteString(strings.Join(req.AuthorizedKeys, "\n") + "\n")
			tmpFile.Close()
			m.injectSSHKeysFromPath(st, tmpFile.Name())
			os.Remove(tmpFile.Name())
		}
	} else {
		m.injectSSHKeys(st)
	}

	// Start the VM
	resp, err := m.startVM(st)
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

	// Ensure dm-snapshot is active
	if err := EnsureSnapshot(vmDir, m.cfg.Storage.GoldenLocalPath); err != nil {
		return nil, fmt.Errorf("ensuring snapshot: %w", err)
	}

	return m.startVM(st)
}

func (m *Manager) startVM(st *VMState) (*CreateResponse, error) {
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

	// Clean stale socket
	os.Remove(filepath.Join(vmDir, "api.sock"))

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
	sshKey := m.cfg.SSH.PrivateKeyPath
	if err := WaitForSSH(st.TAP, sshKey, 30*time.Second); err != nil {
		log.Printf("Warning: SSH not ready for %s: %v", st.Name, err)
	}

	// Join Tailscale
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
		m.injectProxyEnv(st)
	}

	// Clone repo if specified
	if st.CloneURL != "" {
		m.cloneRepo(st)
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

	return map[string]interface{}{
		"hostname":          hostname,
		"vcpu_total":        cpuCount,
		"ram_total_mib":     sysRAM,
		"ram_allocated_mib": totalRAM,
		"ram_free_mib":      sysRAM - totalRAM,
		"vms_total":         len(vms),
		"vms_running":       running,
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

func (m *Manager) cloneRepo(st *VMState) {
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
		return
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

	// Ensure snapshot is active
	if err := EnsureSnapshot(vmDir, m.cfg.Storage.GoldenLocalPath); err != nil {
		return nil, err
	}

	// Write Firecracker config
	if err := writeFirecrackerConfig(vmDir, st); err != nil {
		return nil, err
	}

	return m.startVM(st)
}
