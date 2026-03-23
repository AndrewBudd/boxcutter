package vm

import (
	"syscall"
	"time"
)

// VMBackend abstracts the hypervisor-specific operations for a VM.
// Implementations: FirecrackerBackend (fcbackend.go) and QEMUBackend (qemubackend.go).
type VMBackend interface {
	// Launch starts the VM process. Returns the PID.
	// TAP device is already set up before this is called.
	Launch(vmDir string, st *VMState) (pid int, err error)

	// LaunchIncoming starts a VM in migration-receive mode.
	// The VM is paused, waiting for state to be loaded via RestoreState.
	LaunchIncoming(vmDir string, st *VMState) (pid int, err error)

	// Pause freezes vCPUs (for consistent snapshots/copies).
	Pause(vmDir string) error

	// Resume unfreezes vCPUs after a Pause.
	Resume(vmDir string) error

	// GracefulShutdown attempts a clean shutdown, then force-kills after timeout.
	GracefulShutdown(vmDir string, pid int, timeout time.Duration) error

	// CaptureState saves full VM state (CPU + devices + RAM) to a file.
	// Returns the path to the state file.
	CaptureState(vmDir string) (statePath string, err error)

	// RestoreState loads a saved state file into a VM started with LaunchIncoming.
	RestoreState(vmDir, statePath string) error

	// CreateDisk creates the VM's root disk from the golden image.
	CreateDisk(vmDir, goldenPath, diskSize string) error

	// PrepareDisk mounts the rootfs and injects backend-specific files
	// (kernel modules, Docker config for QEMU; FC config for Firecracker).
	PrepareDisk(mgr *Manager, st *VMState)

	// DiskName returns the rootfs filename ("rootfs.ext4" or "rootfs.qcow2").
	DiskName(vmDir string) string

	// SyncBeforeCopy runs any pre-copy sync (QEMU: sudo sync; FC: pause via Pause()).
	// Returns true if the VM was paused and needs Resume after copy.
	SyncBeforeCopy(st *VMState, sshKey string) (paused bool, err error)
}

// BackendFor returns the appropriate backend for a VM type.
func BackendFor(vmType string) VMBackend {
	if vmType == "qemu" {
		return &QEMUBackend{}
	}
	return &FirecrackerBackend{}
}

// BackendForVM returns the backend for an existing VM based on its state.
func BackendForVM(vmDir string) VMBackend {
	st, err := LoadVMState(vmDir)
	if err != nil || st.Type != "qemu" {
		return &FirecrackerBackend{}
	}
	return &QEMUBackend{}
}

// waitForExit waits for a process to exit, checking every 100ms.
// Shared by both backends for the force-kill fallback.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := killCheck(pid); err != nil {
			return true // process exited
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false // still alive
}

// killCheck returns nil if the process is alive, error if dead.
func killCheck(pid int) error {
	return syscall.Kill(pid, 0)
}
