package vm

import (
	"fmt"
	"os/exec"
	"time"
)

// VMSSH runs a command inside a VM via SSH over the TAP device.
func VMSSH(tap, sshKey string, args ...string) (string, error) {
	proxyCmd := fmt.Sprintf("socat - TCP:10.0.0.2:22,so-bindtodevice=%s", tap)
	sshArgs := []string{
		"-o", "ProxyCommand=" + proxyCmd,
		"-i", sshKey,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"dev@10.0.0.2",
	}
	sshArgs = append(sshArgs, args...)

	cmd := exec.Command("ssh", sshArgs...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// WaitForSSH polls until SSH is ready on the VM.
func WaitForSSH(tap, sshKey string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := VMSSH(tap, sshKey, "echo", "ready")
		if err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("SSH not ready after %s", timeout)
}
