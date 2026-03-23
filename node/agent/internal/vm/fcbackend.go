package vm

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// FirecrackerBackend implements VMBackend for Firecracker microVMs.
type FirecrackerBackend struct{}

func (f *FirecrackerBackend) Launch(vmDir string, st *VMState) (int, error) {
	os.Remove(filepath.Join(vmDir, "api.sock"))
	os.Remove(filepath.Join(vmDir, "vsock.sock"))

	logFile, _ := os.Create(filepath.Join(vmDir, "firecracker.log"))
	cmd := exec.Command("firecracker",
		"--api-sock", filepath.Join(vmDir, "api.sock"),
		"--config-file", filepath.Join(vmDir, "fc-config.json"),
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return 0, fmt.Errorf("starting firecracker: %w", err)
	}
	logFile.Close()

	pid := cmd.Process.Pid
	os.WriteFile(filepath.Join(vmDir, "firecracker.pid"),
		[]byte(fmt.Sprintf("%d", pid)), 0644)
	go cmd.Wait()

	return pid, nil
}

func (f *FirecrackerBackend) LaunchIncoming(vmDir string, st *VMState) (int, error) {
	// Firecracker uses ImportSnapshot (different flow from QEMU incoming).
	// This is called from the ImportSnapshot path which handles it directly.
	// For the unified migration, we don't use this — FC uses its own import path.
	return 0, fmt.Errorf("Firecracker uses ImportSnapshot, not LaunchIncoming")
}

func (f *FirecrackerBackend) Pause(vmDir string) error {
	return fcPause(vmDir)
}

func (f *FirecrackerBackend) Resume(vmDir string) error {
	return fcResume(vmDir)
}

func (f *FirecrackerBackend) GracefulShutdown(vmDir string, pid int, timeout time.Duration) error {
	// Try CtrlAltDel via Firecracker API
	if !IsMigrating(vmDir) {
		apiSock := filepath.Join(vmDir, "api.sock")
		if _, err := os.Stat(apiSock); err == nil {
			run("curl", "-s", "--unix-socket", apiSock,
				"-X", "PUT", "http://localhost/actions",
				"-H", "Content-Type: application/json",
				"-d", `{"action_type":"SendCtrlAltDel"}`)

			if waitForExit(pid, timeout) {
				log.Printf("stopVM: PID %d exited after CtrlAltDel", pid)
				return nil
			}
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

func (f *FirecrackerBackend) CaptureState(vmDir string) (string, error) {
	snapPath, _, err := fcSnapshot(vmDir)
	if err != nil {
		return "", err
	}
	// Return the directory containing both vm.snap and vm.mem
	return filepath.Dir(snapPath), nil
}

func (f *FirecrackerBackend) RestoreState(vmDir, statePath string) error {
	// Firecracker restore is handled via ImportSnapshot which launches
	// the FC process with the snapshot. This method isn't used in the
	// unified migration path for FC (it uses its own import flow).
	return fmt.Errorf("Firecracker uses ImportSnapshot for state restore")
}

func (f *FirecrackerBackend) CreateDisk(vmDir, goldenPath, diskSize string) error {
	return CreateRootfs(vmDir, goldenPath, diskSize)
}

func (f *FirecrackerBackend) PrepareDisk(mgr *Manager, st *VMState) {
	vmDir := VMDir(st.Name)
	writeFirecrackerConfig(vmDir, st)
}

func (f *FirecrackerBackend) DiskName(vmDir string) string {
	return "rootfs.ext4"
}

func (f *FirecrackerBackend) SyncBeforeCopy(st *VMState, sshKey string) (bool, error) {
	vmDir := VMDir(st.Name)
	if err := fcPause(vmDir); err != nil {
		return false, fmt.Errorf("pausing: %w", err)
	}
	return true, nil // paused, needs Resume after copy
}
