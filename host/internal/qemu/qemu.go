// Package qemu manages QEMU VM processes.
package qemu

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type VMConfig struct {
	Name string
	VCPU int
	RAM  string
	Disk string
	ISO  string
	TAP  string
	MAC  string
}

// Launch starts a QEMU VM in daemon mode. Returns the PID.
func Launch(cfg VMConfig, logDir string) (int, error) {
	if !fileExists(cfg.Disk) {
		return 0, fmt.Errorf("disk not found: %s", cfg.Disk)
	}
	if !fileExists(cfg.ISO) {
		return 0, fmt.Errorf("cloud-init ISO not found: %s", cfg.ISO)
	}

	pidFile := filepath.Join(logDir, cfg.Name+".pid")
	consoleLog := filepath.Join(logDir, cfg.Name+"-console.log")

	// Check if already running
	if pid := readPID(pidFile); pid > 0 {
		if processRunning(pid) {
			log.Printf("  %s already running (PID %d)", cfg.Name, pid)
			return pid, nil
		}
		os.Remove(pidFile)
	}

	args := []string{
		"-enable-kvm",
		"-cpu", "host",
		"-smp", strconv.Itoa(cfg.VCPU),
		"-m", cfg.RAM,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", cfg.Disk),
		"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio", cfg.ISO),
		"-netdev", fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", cfg.TAP),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", cfg.MAC),
		"-display", "none",
		"-serial", fmt.Sprintf("file:%s", consoleLog),
		"-daemonize",
		"-pidfile", pidFile,
	}

	log.Printf("  Launching %s (vcpu=%d ram=%s tap=%s)", cfg.Name, cfg.VCPU, cfg.RAM, cfg.TAP)
	cmd := exec.Command("qemu-system-x86_64", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("qemu launch: %s: %s", err, strings.TrimSpace(string(out)))
	}

	pid := readPID(pidFile)
	if pid <= 0 {
		return 0, fmt.Errorf("qemu launched but no PID file created")
	}

	log.Printf("  %s started (PID %d)", cfg.Name, pid)
	return pid, nil
}

// Stop sends SIGTERM to a QEMU process, waits, then SIGKILL if needed.
func Stop(name string, pid int) error {
	if pid <= 0 || !processRunning(pid) {
		return nil
	}

	log.Printf("  Stopping %s (PID %d)", name, pid)
	syscall.Kill(pid, syscall.SIGTERM)

	for i := 0; i < 15; i++ {
		if !processRunning(pid) {
			log.Printf("  %s stopped", name)
			return nil
		}
		time.Sleep(time.Second)
	}

	syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(time.Second)
	log.Printf("  %s killed", name)
	return nil
}

// IsRunning checks if a process with the given PID exists.
func IsRunning(pid int) bool {
	return pid > 0 && processRunning(pid)
}

func processRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func readPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
