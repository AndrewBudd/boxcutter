package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// VMState is the persistent state for a VM, stored as vm.json.
type VMState struct {
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"`        // "firecracker" (default/empty) or "qemu"
	Description string   `json:"description,omitempty"` // user-provided description
	VCPU        int      `json:"vcpu"`
	RAMMIB      int      `json:"ram_mib"`
	Mark        int      `json:"mark"`
	Mode        string   `json:"mode"`
	MAC         string   `json:"mac"`
	Disk        string   `json:"disk"`
	TAP         string   `json:"tap"`
	Created     string   `json:"created"`
	CloneURL    string   `json:"clone_url,omitempty"`    // first/primary clone URL (backwards compat)
	CloneURLs   []string `json:"clone_urls,omitempty"`   // all clone URLs
	GitHubRepo  string   `json:"github_repo,omitempty"`  // backwards compat — single repo
	GitHubRepos    []string `json:"github_repos,omitempty"`    // all GitHub repos (owner/repo)
	GitHubProjects []string `json:"github_projects,omitempty"` // GitHub projects (owner/number)
	TailscaleIP      string          `json:"tailscale_ip,omitempty"`
	TailscaleAuthkey string          `json:"tailscale_authkey,omitempty"`
	GoldenVer        string          `json:"golden_version,omitempty"`
	AgentConfig      json.RawMessage `json:"agent_config,omitempty"` // opaque JSON blob forwarded to vmid
}

// AllGitHubRepos returns the list of GitHub repos, falling back to the single
// GitHubRepo field for backwards compatibility with older vm.json files.
func (st *VMState) AllGitHubRepos() []string {
	if len(st.GitHubRepos) > 0 {
		return st.GitHubRepos
	}
	if st.GitHubRepo != "" {
		return []string{st.GitHubRepo}
	}
	return nil
}

// AllCloneURLs returns the list of clone URLs, falling back to the single
// CloneURL field for backwards compatibility.
func (st *VMState) AllCloneURLs() []string {
	if len(st.CloneURLs) > 0 {
		return st.CloneURLs
	}
	if st.CloneURL != "" {
		return []string{st.CloneURL}
	}
	return nil
}

// IsMigrating checks if the VM has an in-flight migration marker.
// This is a filesystem marker (not persisted state) that exists only during
// the migration operation.
func IsMigrating(vmDir string) bool {
	_, err := os.Stat(filepath.Join(vmDir, "migrating"))
	return err == nil
}

// TrySetMigrating atomically creates the migration marker file using O_CREATE|O_EXCL.
// Returns true if the marker was created (no migration was in progress).
// Returns false if the marker already exists (migration already in progress).
// This replaces the old in-memory migratingSet — the filesystem IS the state.
func TrySetMigrating(vmDir string, target string) bool {
	marker := filepath.Join(vmDir, "migrating")
	content := "1"
	if target != "" {
		content = target
	}
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return false // file already exists or dir missing — either way, not started
	}
	f.Write([]byte(content))
	f.Close()
	return true
}

// ClearMigrating removes the migration marker file.
func ClearMigrating(vmDir string) {
	os.Remove(filepath.Join(vmDir, "migrating"))
}

// SetMigrating creates or removes the migration marker file.
// When migrating is true, the marker stores the target address (addr:port)
// so that crash recovery can check if the target already has the VM.
func SetMigrating(vmDir string, migrating bool, target ...string) {
	marker := filepath.Join(vmDir, "migrating")
	if migrating {
		content := "1"
		if len(target) > 0 && target[0] != "" {
			content = target[0]
		}
		os.WriteFile(marker, []byte(content), 0644)
	} else {
		os.Remove(marker)
	}
}

// MigrationTarget reads the target address from the migration marker file.
// Returns empty string if not migrating or if marker has no target info.
func MigrationTarget(vmDir string) string {
	data, err := os.ReadFile(filepath.Join(vmDir, "migrating"))
	if err != nil {
		return ""
	}
	target := strings.TrimSpace(string(data))
	if target == "1" || target == "" {
		return ""
	}
	return target
}

// DeriveStatus computes VM status from reality, never from stored state.
// Source of truth: process existence + migration/migrated markers.
func DeriveStatus(vmDir string) string {
	if IsMigrating(vmDir) {
		return "migrating"
	}
	if IsMigrated(vmDir) {
		return "migrated"
	}
	if IsRunning(vmDir) {
		return "running"
	}
	return "stopped"
}

// IsMigrated checks if the VM has completed migration (source copy).
func IsMigrated(vmDir string) bool {
	_, err := os.Stat(filepath.Join(vmDir, "migrated"))
	return err == nil
}

// SetMigrated creates or removes the migrated marker on the source VM.
func SetMigrated(vmDir string, migrated bool) {
	marker := filepath.Join(vmDir, "migrated")
	if migrated {
		os.WriteFile(marker, []byte("1"), 0644)
	} else {
		os.Remove(marker)
	}
}

// SnapshotState tracks dm-snapshot loop devices.
type SnapshotState struct {
	BaseLoop string `json:"base_loop"`
	CowLoop  string `json:"cow_loop"`
	DMName   string `json:"dm_name"`
}

// IsFileRootfs returns true if the VM uses a standalone rootfs file
// instead of dm-snapshot. Detected by the presence of rootfs.ext4.
func IsFileRootfs(vmDir string) bool {
	if _, err := os.Stat(filepath.Join(vmDir, "rootfs.qcow2")); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(vmDir, "rootfs.ext4"))
	return err == nil
}

// RootfsPath returns the block device or file path for this VM's rootfs.
// Checks for QCOW2 first (QEMU VMs), then ext4, then dm-snapshot.
func RootfsPath(vmDir string) string {
	qcow2Path := filepath.Join(vmDir, "rootfs.qcow2")
	if _, err := os.Stat(qcow2Path); err == nil {
		return qcow2Path
	}
	if IsFileRootfs(vmDir) {
		return filepath.Join(vmDir, "rootfs.ext4")
	}
	return "/dev/mapper/bc-" + filepath.Base(vmDir)
}

// DiskFormat returns "qcow2", "raw", or "dm" based on what rootfs exists.
func DiskFormat(vmDir string) string {
	if _, err := os.Stat(filepath.Join(vmDir, "rootfs.qcow2")); err == nil {
		return "qcow2"
	}
	if IsFileRootfs(vmDir) {
		return "raw"
	}
	return "dm"
}

func LoadVMState(vmDir string) (*VMState, error) {
	data, err := os.ReadFile(filepath.Join(vmDir, "vm.json"))
	if err != nil {
		return nil, err
	}
	var st VMState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func SaveVMState(vmDir string, st *VMState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(vmDir, "vm.json"), data, 0644)
}

func LoadSnapshotState(vmDir string) (*SnapshotState, error) {
	data, err := os.ReadFile(filepath.Join(vmDir, "snapshot.json"))
	if err != nil {
		return nil, err
	}
	var ss SnapshotState
	if err := json.Unmarshal(data, &ss); err != nil {
		return nil, err
	}
	return &ss, nil
}

func SaveSnapshotState(vmDir string, ss *SnapshotState) error {
	data, err := json.MarshalIndent(ss, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(vmDir, "snapshot.json"), data, 0644)
}

// VMDir returns the directory for a named VM.
func VMDir(name string) string {
	return filepath.Join("/var/lib/boxcutter/vms", name)
}

// ListVMs returns all VM states from disk.
func ListVMs() ([]*VMState, error) {
	vmBase := "/var/lib/boxcutter/vms"
	entries, err := os.ReadDir(vmBase)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var vms []*VMState
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := LoadVMState(filepath.Join(vmBase, e.Name()))
		if err != nil {
			continue
		}
		vms = append(vms, st)
	}
	return vms, nil
}

// IsRunning checks if the VM process is alive (Firecracker or QEMU).
func IsRunning(vmDir string) bool {
	pid := ReadPID(vmDir)
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// ReadPID returns the VM process PID, checking both QEMU and Firecracker PID files.
func ReadPID(vmDir string) int {
	for _, pidFile := range []string{"qemu.pid", "firecracker.pid"} {
		data, err := os.ReadFile(filepath.Join(vmDir, pidFile))
		if err != nil {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		if syscall.Kill(pid, 0) == nil {
			return pid
		}
	}
	return 0
}

// IsQEMU returns true if this VM directory contains a QEMU VM.
func IsQEMU(vmDir string) bool {
	st, err := LoadVMState(vmDir)
	if err != nil {
		return false
	}
	return st.Type == "qemu"
}

// PIDFile returns the PID file name for the given VM type.
func PIDFile(vmType string) string {
	if vmType == "qemu" {
		return "qemu.pid"
	}
	return "firecracker.pid"
}
