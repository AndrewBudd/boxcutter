package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// VMState is the persistent state for a VM, stored as vm.json.
type VMState struct {
	Name        string `json:"name"`
	VCPU        int    `json:"vcpu"`
	RAMMIB      int    `json:"ram_mib"`
	Mark        int    `json:"mark"`
	Mode        string `json:"mode"`
	MAC         string `json:"mac"`
	Disk        string `json:"disk"`
	TAP         string `json:"tap"`
	Created     string `json:"created"`
	CloneURL    string `json:"clone_url,omitempty"`
	GitHubRepo  string `json:"github_repo,omitempty"`
	TailscaleIP string `json:"tailscale_ip,omitempty"`
	GoldenVer   string `json:"golden_version,omitempty"`
}

// SnapshotState tracks dm-snapshot loop devices.
type SnapshotState struct {
	BaseLoop string `json:"base_loop"`
	CowLoop  string `json:"cow_loop"`
	DMName   string `json:"dm_name"`
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
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 tests if process exists
	return process.Signal(nil) == nil
}
