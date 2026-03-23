package vm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// qmpResponse is a generic QMP response.
type qmpResponse struct {
	Return json.RawMessage        `json:"return,omitempty"`
	Error  *qmpError              `json:"error,omitempty"`
	Event  string                 `json:"event,omitempty"`
	QMP    map[string]interface{} `json:"QMP,omitempty"`
}

type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

// qmpDial connects to the QMP socket and completes capability negotiation.
func qmpDial(vmDir string) (net.Conn, error) {
	sockPath := filepath.Join(vmDir, "qmp.sock")
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("qmp dial %s: %w", sockPath, err)
	}

	reader := bufio.NewReader(conn)

	// Read the QMP greeting
	line, err := reader.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp greeting: %w", err)
	}

	var greeting qmpResponse
	if err := json.Unmarshal(line, &greeting); err != nil || greeting.QMP == nil {
		conn.Close()
		return nil, fmt.Errorf("qmp invalid greeting: %s", string(line))
	}

	// Send qmp_capabilities to exit negotiation mode
	if _, err := conn.Write([]byte(`{"execute":"qmp_capabilities"}` + "\n")); err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp capabilities send: %w", err)
	}

	// Read the response
	line, err = reader.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp capabilities response: %w", err)
	}

	return conn, nil
}

// qmpCommand sends a QMP command and returns the result.
// Skips async event messages while waiting for the return.
func qmpCommand(conn net.Conn, execute string, arguments map[string]interface{}) (json.RawMessage, error) {
	cmd := map[string]interface{}{"execute": execute}
	if arguments != nil {
		cmd["arguments"] = arguments
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')

	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("qmp write: %w", err)
	}

	reader := bufio.NewReader(conn)
	for {
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("qmp read: %w", err)
		}

		var resp qmpResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			return nil, fmt.Errorf("qmp parse: %w", err)
		}

		// Skip async events
		if resp.Event != "" {
			continue
		}

		if resp.Error != nil {
			return nil, fmt.Errorf("qmp error: %s: %s", resp.Error.Class, resp.Error.Desc)
		}

		return resp.Return, nil
	}
}

// qmpStop pauses vCPUs (equivalent to fcPause).
func qmpStop(vmDir string) error {
	conn, err := qmpDial(vmDir)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = qmpCommand(conn, "stop", nil)
	return err
}

// qmpCont resumes vCPUs (equivalent to fcResume).
func qmpCont(vmDir string) error {
	conn, err := qmpDial(vmDir)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = qmpCommand(conn, "cont", nil)
	return err
}

// qmpSaveState saves full VM state (CPU + devices + RAM) to a file.
// Uses the QMP migrate command with file: URI.
// Returns the path to the state file.
func qmpSaveState(vmDir string) (string, error) {
	conn, err := qmpDial(vmDir)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// Choose destination: /dev/shm if enough space, otherwise vmDir
	statePath := filepath.Join(vmDir, "qemu-state.bin")

	// Check /dev/shm space
	shmDir := filepath.Join("/dev/shm", "bc-"+filepath.Base(vmDir)+"-mig")
	if shmFree := getShmFree(); shmFree > 0 {
		// Need roughly RAM size for the state file
		pidData, _ := os.ReadFile(filepath.Join(vmDir, "qemu.pid"))
		var pid int
		fmt.Sscanf(string(pidData), "%d", &pid)
		if pid > 0 {
			// Estimate state size from /proc/pid/status VmRSS
			vmRSS := getVMRSS(pid)
			if vmRSS > 0 && shmFree > vmRSS*12/10 { // 1.2x safety margin
				os.MkdirAll(shmDir, 0755)
				statePath = filepath.Join(shmDir, "qemu-state.bin")
			}
		}
	}

	log.Printf("qmpSaveState: saving to %s", statePath)

	// Issue migrate command
	_, err = qmpCommand(conn, "migrate", map[string]interface{}{
		"uri": "file:" + statePath,
	})
	if err != nil {
		return "", fmt.Errorf("qmp migrate: %w", err)
	}

	// Poll for completion
	for i := 0; i < 600; i++ { // up to 5 minutes
		time.Sleep(500 * time.Millisecond)
		result, err := qmpCommand(conn, "query-migrate", nil)
		if err != nil {
			return "", fmt.Errorf("qmp query-migrate: %w", err)
		}

		var status struct {
			Status string `json:"status"`
		}
		json.Unmarshal(result, &status)

		switch status.Status {
		case "completed":
			log.Printf("qmpSaveState: completed")
			return statePath, nil
		case "failed":
			return "", fmt.Errorf("qmp migrate failed")
		case "active", "setup":
			continue
		default:
			continue
		}
	}

	return "", fmt.Errorf("qmp migrate timed out after 5 minutes")
}

// getShmFree returns free bytes on /dev/shm, or 0 if unavailable.
func getShmFree() int64 {
	// Use a simple approach
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	// Parse Shmem line or just check df
	// Simplified: use 50% of total RAM as /dev/shm estimate
	for _, line := range splitLines(string(data)) {
		if len(line) > 10 && line[:7] == "MemFree" {
			var kb int64
			fmt.Sscanf(line, "MemFree: %d kB", &kb)
			return kb * 1024
		}
	}
	return 0
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// getVMRSS returns the resident memory of a process in bytes.
func getVMRSS(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range splitLines(string(data)) {
		if len(line) > 6 && line[:5] == "VmRSS" {
			var kb int64
			fmt.Sscanf(line, "VmRSS: %d kB", &kb)
			return kb * 1024
		}
	}
	return 0
}

// qmpWaitForSocket waits for the QMP socket to become available.
func qmpWaitForSocket(vmDir string, timeout time.Duration) error {
	sockPath := filepath.Join(vmDir, "qmp.sock")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("qmp socket not available after %s", timeout)
}

// launchQEMUIncoming starts a QEMU VM in incoming migration mode.
// The VM is created paused, waiting for state to be loaded.
func launchQEMUIncoming(vmDir string, st *VMState) (int, error) {
	rootfs := RootfsPath(vmDir)
	logPath := filepath.Join(vmDir, "console.log")
	pidFile := filepath.Join(vmDir, "qemu.pid")

	kernel, initrd := findQEMUKernel()
	if kernel == "" {
		return 0, fmt.Errorf("no kernel found for QEMU VM")
	}

	bootArgs := fmt.Sprintf(
		"console=ttyS0 root=/dev/vda rw init=/sbin/init "+
			"net.ifnames=0 biosdevname=0 "+
			"ip=10.0.0.2::10.0.0.1:255.255.255.252:%s:eth0:off:8.8.8.8",
		st.Name)

	// Detect disk format
	diskFormat := "raw"
	if _, err := os.Stat(filepath.Join(vmDir, "rootfs.qcow2")); err == nil {
		diskFormat = "qcow2"
	}

	args := []string{
		"-enable-kvm",
		"-cpu", "host",
		"-smp", fmt.Sprintf("%d", st.VCPU),
		"-m", fmt.Sprintf("%d", st.RAMMIB),
		"-kernel", kernel,
		"-append", bootArgs,
		"-drive", fmt.Sprintf("file=%s,format=%s,if=virtio,cache=writeback", rootfs, diskFormat),
		"-netdev", fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", st.TAP),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", st.MAC),
		"-serial", fmt.Sprintf("file:%s", logPath),
		"-display", "none",
		"-no-reboot",
		"-pidfile", pidFile,
		"-qmp", fmt.Sprintf("unix:%s,server,nowait", filepath.Join(vmDir, "qmp.sock")),
		"-incoming", "defer",
		"-daemonize",
	}

	if initrd != "" {
		args = append(args, "-initrd", initrd)
	}

	log.Printf("Launching QEMU VM %s (incoming migration mode)", st.Name)

	cmd := exec.Command("qemu-system-x86_64", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("qemu-system-x86_64 incoming: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Wait for PID file
	var pid int
	for i := 0; i < 20; i++ {
		data, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
			if pid > 0 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pid == 0 {
		return 0, fmt.Errorf("QEMU incoming: PID file not found")
	}

	// Wait for QMP socket
	if err := qmpWaitForSocket(vmDir, 10*time.Second); err != nil {
		return 0, fmt.Errorf("QEMU incoming: %w", err)
	}

	log.Printf("QEMU VM %s ready for incoming migration (PID %d)", st.Name, pid)
	return pid, nil
}

// qmpLoadState loads a saved state file into a QEMU VM started with -incoming defer.
func qmpLoadState(vmDir, statePath string) error {
	conn, err := qmpDial(vmDir)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Tell QEMU to load from the state file
	_, err = qmpCommand(conn, "migrate-incoming", map[string]interface{}{
		"uri": "file:" + statePath,
	})
	if err != nil {
		return fmt.Errorf("qmp migrate-incoming: %w", err)
	}

	// Poll until loaded
	for i := 0; i < 600; i++ {
		time.Sleep(500 * time.Millisecond)
		result, err := qmpCommand(conn, "query-migrate", nil)
		if err != nil {
			// After successful load, query-migrate may return empty
			// Check if VM is running
			statusResult, statusErr := qmpCommand(conn, "query-status", nil)
			if statusErr == nil {
				var qs struct {
					Status  string `json:"status"`
					Running bool   `json:"running"`
				}
				json.Unmarshal(statusResult, &qs)
				if qs.Status == "postmigrate" || qs.Status == "paused" {
					// State loaded, VM paused. Resume it.
					_, err = qmpCommand(conn, "cont", nil)
					return err
				}
			}
			continue
		}

		var status struct {
			Status string `json:"status"`
		}
		json.Unmarshal(result, &status)

		switch status.Status {
		case "completed":
			// Resume the VM
			_, err = qmpCommand(conn, "cont", nil)
			return err
		case "failed":
			return fmt.Errorf("qmp incoming migration failed")
		}
	}

	return fmt.Errorf("qmp incoming migration timed out")
}
