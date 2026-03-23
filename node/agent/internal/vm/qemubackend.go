package vm

import (
	"fmt"
	"log"
	"os/exec"
	"syscall"
	"time"
)

// QEMUBackend implements VMBackend for QEMU VMs.
type QEMUBackend struct{}

func (q *QEMUBackend) Launch(vmDir string, st *VMState) (int, error) {
	return launchQEMU(vmDir, st)
}

func (q *QEMUBackend) LaunchIncoming(vmDir string, st *VMState) (int, error) {
	return launchQEMUIncoming(vmDir, st)
}

func (q *QEMUBackend) Pause(vmDir string) error {
	return qmpStop(vmDir)
}

func (q *QEMUBackend) Resume(vmDir string) error {
	return qmpCont(vmDir)
}

func (q *QEMUBackend) GracefulShutdown(vmDir string, pid int, timeout time.Duration) error {
	// QEMU: SIGTERM triggers ACPI shutdown
	if !IsMigrating(vmDir) {
		syscall.Kill(pid, syscall.SIGTERM)
		if waitForExit(pid, timeout) {
			log.Printf("stopVM: PID %d exited after SIGTERM", pid)
			return nil
		}
	}

	// Force kill
	syscall.Kill(pid, syscall.SIGKILL)
	if waitForExit(pid, 5*time.Second) {
		return nil
	}
	exec.Command("kill", "-9", fmt.Sprint(pid)).Run()
	waitForExit(pid, 2*time.Second)
	return nil
}

func (q *QEMUBackend) CaptureState(vmDir string) (string, error) {
	return qmpSaveState(vmDir)
}

func (q *QEMUBackend) RestoreState(vmDir, statePath string) error {
	return qmpLoadState(vmDir, statePath)
}

func (q *QEMUBackend) CreateDisk(vmDir, goldenPath, diskSize string) error {
	return CreateQCOW2Rootfs(vmDir, goldenPath, diskSize)
}

func (q *QEMUBackend) PrepareDisk(mgr *Manager, st *VMState) {
	mgr.prepareRootfsForQEMU(st)
}

func (q *QEMUBackend) DiskName(vmDir string) string {
	if DiskFormat(vmDir) == "qcow2" {
		return "rootfs.qcow2"
	}
	return "rootfs.ext4" // legacy QEMU VMs with raw ext4
}

func (q *QEMUBackend) SyncBeforeCopy(st *VMState, sshKey string) (bool, error) {
	// QEMU: sync filesystem, VM stays running (crash-consistent copy)
	VMSSH(st.TAP, sshKey, "sudo sync")
	return false, nil // not paused, no Resume needed
}
