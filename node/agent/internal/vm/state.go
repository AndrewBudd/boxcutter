package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// VMState is the persistent state for a VM, stored as vm.json.
type VMState struct {
	Name        string   `json:"name"`
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
	GitHubRepos []string `json:"github_repos,omitempty"` // all GitHub repos (owner/repo)
	TailscaleIP string   `json:"tailscale_ip,omitempty"`
	GoldenVer   string   `json:"golden_version,omitempty"`
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

// SetMigrating creates or removes the migration marker file.
func SetMigrating(vmDir string, migrating bool) {
	marker := filepath.Join(vmDir, "migrating")
	if migrating {
		os.WriteFile(marker, []byte("1"), 0644)
	} else {
		os.Remove(marker)
	}
}

// DeriveStatus computes VM status from reality, never from stored state.
// Source of truth: process existence + migration marker.
func DeriveStatus(vmDir string) string {
	if IsMigrating(vmDir) {
		return "migrating"
	}
	if IsRunning(vmDir) {
		return "running"
	}
	return "stopped"
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
	_, err := os.Stat(filepath.Join(vmDir, "rootfs.ext4"))
	return err == nil
}

// RootfsPath returns the block device or file path for this VM's rootfs.
func RootfsPath(vmDir string) string {
	if IsFileRootfs(vmDir) {
		return filepath.Join(vmDir, "rootfs.ext4")
	}
	return "/dev/mapper/bc-" + filepath.Base(vmDir)
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

// IsRunning checks if the Firecracker process is alive.
func IsRunning(vmDir string) bool {
	data, err := os.ReadFile(filepath.Join(vmDir, "firecracker.pid"))
	if err != nil {
		return false
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil || pid <= 0 {
		return false
	}
	// Signal 0 tests if process exists
	return syscall.Kill(pid, 0) == nil
}
