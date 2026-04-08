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
	"syscall"
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
			Status   string `json:"status"`
			ErrorDesc string `json:"error-desc"`
		}
		json.Unmarshal(result, &status)

		switch status.Status {
		case "completed":
			log.Printf("qmpSaveState: completed")
			return statePath, nil
		case "failed":
			log.Printf("qmpSaveState: full query-migrate response: %s", string(result))
			if status.ErrorDesc != "" {
				return "", fmt.Errorf("qmp migrate failed: %s", status.ErrorDesc)
			}
			return "", fmt.Errorf("qmp migrate failed (no error-desc in response: %s)", string(result))
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

	kernel, initrd, err := localKernel(vmDir)
	if err != nil {
		return 0, fmt.Errorf("no kernel found for QEMU VM: %w", err)
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

	qemuLogPath := filepath.Join(vmDir, "qemu-daemon.log")
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
		"-D", qemuLogPath,
		"-incoming", "defer",
		// No -daemonize: manage process directly for crash detection
	}

	if initrd != "" {
		args = append(args, "-initrd", initrd)
	}

	// Attach tools.img — must match source VM's device topology for migration.
	if _, err := os.Stat(toolsImagePath); err == nil {
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=raw,if=virtio,readonly=on", toolsImagePath))
	}

	log.Printf("Launching QEMU VM %s (incoming migration mode)", st.Name)

	// Capture stderr to a log file for migration diagnostics
	stderrLog, _ := os.Create(filepath.Join(vmDir, "qemu-incoming.log"))

	cmd := exec.Command("qemu-system-x86_64", args...)
	cmd.Stdout = stderrLog
	cmd.Stderr = stderrLog
	if err := cmd.Start(); err != nil {
		if stderrLog != nil {
			stderrLog.Close()
		}
		return 0, fmt.Errorf("qemu-system-x86_64 incoming start: %w", err)
	}
	pid := cmd.Process.Pid
	if stderrLog != nil {
		stderrLog.Close()
	}

	// Write PID file ourselves (no -daemonize to do it for us)
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", pid)), 0644)

	// Monitor process in background for crash diagnostics
	go func() {
		err := cmd.Wait()
		if err != nil {
			stderrContent, _ := os.ReadFile(filepath.Join(vmDir, "qemu-incoming.log"))
			dmesgOOM := ""
			if dmesgOut, dErr := exec.Command("dmesg", "--since", "2 minutes ago").Output(); dErr == nil {
				for _, line := range strings.Split(string(dmesgOut), "\n") {
					if strings.Contains(line, "oom") || strings.Contains(line, "OOM") || strings.Contains(line, "Killed process") {
						dmesgOOM += line + "\n"
					}
				}
			}
			log.Printf("QEMU VM %s (incoming defer) exited unexpectedly: %v | stderr: %s | OOM: %s",
				st.Name, err, strings.TrimSpace(string(stderrContent)), strings.TrimSpace(dmesgOOM))
		}
	}()

	// Wait for QMP socket
	if err := qmpWaitForSocket(vmDir, 10*time.Second); err != nil {
		return 0, fmt.Errorf("QEMU incoming: %w", err)
	}

	log.Printf("QEMU VM %s ready for incoming migration (PID %d)", st.Name, pid)
	return pid, nil
}

// launchQEMUIncomingTCP starts a QEMU VM that listens for incoming live migration on a TCP port.
// Returns (pid, port, error). The port is chosen by the OS (port 0 = auto-assign).
func launchQEMUIncomingTCP(vmDir string, st *VMState) (int, int, error) {
	rootfs := RootfsPath(vmDir)
	logPath := filepath.Join(vmDir, "console.log")
	pidFile := filepath.Join(vmDir, "qemu.pid")

	kernel, initrd, err := localKernel(vmDir)
	if err != nil {
		return 0, 0, fmt.Errorf("no kernel found for QEMU VM: %w", err)
	}

	bootArgs := fmt.Sprintf(
		"console=ttyS0 root=/dev/vda rw init=/sbin/init "+
			"net.ifnames=0 biosdevname=0 "+
			"ip=10.0.0.2::10.0.0.1:255.255.255.252:%s:eth0:off:8.8.8.8",
		st.Name)

	diskFormat := "raw"
	if _, err := os.Stat(filepath.Join(vmDir, "rootfs.qcow2")); err == nil {
		diskFormat = "qcow2"
	}

	// Use a fixed port range (49152-49199) to find a free one
	migratePort := 0
	for p := 49152; p < 49200; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err == nil {
			ln.Close()
			migratePort = p
			break
		}
	}
	if migratePort == 0 {
		return 0, 0, fmt.Errorf("no free TCP port for incoming migration")
	}

	qemuLogPath := filepath.Join(vmDir, "qemu-daemon.log")
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
		"-D", qemuLogPath,
		"-incoming", fmt.Sprintf("tcp:0:%d", migratePort),
		// No -daemonize: we manage the process directly so we can detect
		// crashes during live migration (OOM kills, KVM failures, etc.)
		// instead of losing the exit code to the daemonized child.
	}

	if initrd != "" {
		args = append(args, "-initrd", initrd)
	}

	// Attach tools.img — must match source VM's device topology for live migration.
	// QEMU rejects incoming migration if the target has fewer virtio drives than
	// the source, causing silent fallback to stop+relocate.
	if _, err := os.Stat(toolsImagePath); err == nil {
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=raw,if=virtio,readonly=on", toolsImagePath))
		log.Printf("QEMU VM %s (incoming): attached tools.img as /dev/vdb", st.Name)
	}

	log.Printf("Launching QEMU VM %s (incoming live migration on port %d)", st.Name, migratePort)

	stderrLog, _ := os.Create(filepath.Join(vmDir, "qemu-incoming.log"))
	cmd := exec.Command("qemu-system-x86_64", args...)
	cmd.Stdout = stderrLog // capture any stdout too
	cmd.Stderr = stderrLog
	if err := cmd.Start(); err != nil {
		if stderrLog != nil {
			stderrLog.Close()
		}
		return 0, 0, fmt.Errorf("qemu incoming tcp start: %w", err)
	}
	pid := cmd.Process.Pid
	if stderrLog != nil {
		stderrLog.Close()
	}

	// Write PID file (QEMU would have written it with -daemonize, but
	// since we manage the process directly, write it ourselves).
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", pid)), 0644)

	// Monitor process in background — log crash details (exit code, OOM, stderr)
	// so migration failures have actionable diagnostics instead of "VM not running".
	go func() {
		err := cmd.Wait()
		if err != nil {
			stderrContent, _ := os.ReadFile(filepath.Join(vmDir, "qemu-incoming.log"))
			dmesgOOM := ""
			if dmesgOut, dErr := exec.Command("dmesg", "--since", "2 minutes ago").Output(); dErr == nil {
				for _, line := range strings.Split(string(dmesgOut), "\n") {
					if strings.Contains(line, "oom") || strings.Contains(line, "OOM") || strings.Contains(line, "Killed process") {
						dmesgOOM += line + "\n"
					}
				}
			}
			log.Printf("QEMU VM %s (incoming migration) exited unexpectedly: %v | stderr: %s | OOM: %s",
				st.Name, err, strings.TrimSpace(string(stderrContent)), strings.TrimSpace(dmesgOOM))
		}
	}()

	// Wait for QMP socket first — this is a necessary but not sufficient condition.
	if err := qmpWaitForSocket(vmDir, 10*time.Second); err != nil {
		stderrContent, _ := os.ReadFile(filepath.Join(vmDir, "qemu-incoming.log"))
		return 0, 0, fmt.Errorf("QEMU incoming TCP: %w (stderr: %s)", err, strings.TrimSpace(string(stderrContent)))
	}

	// Verify QEMU is still alive after init
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		stderrContent, _ := os.ReadFile(filepath.Join(vmDir, "qemu-incoming.log"))
		return 0, 0, fmt.Errorf("QEMU incoming TCP died after init: %s", strings.TrimSpace(string(stderrContent)))
	}

	// Wait for the migration TCP port to actually be in LISTEN state.
	// QMP socket readiness does NOT guarantee the TCP port is bound — under
	// nested virtualization or load, QEMU may open QMP before binding the
	// incoming migration port, causing "Connection refused" on the source.
	// Do NOT probe by connecting: QEMU treats any TCP connection as a migration
	// stream, and a probe crashes the target with "Not a migration stream".
	// Instead, check /proc/net/tcp for the port in LISTEN state (safe, no connection).
	if err := waitForPortListen(migratePort, 10*time.Second); err != nil {
		stderrContent, _ := os.ReadFile(filepath.Join(vmDir, "qemu-incoming.log"))
		return 0, 0, fmt.Errorf("QEMU incoming TCP port not listening: %w (stderr: %s)", err, strings.TrimSpace(string(stderrContent)))
	}

	log.Printf("QEMU VM %s ready for incoming live migration on port %d (PID %d)", st.Name, migratePort, pid)
	return pid, migratePort, nil
}

// qmpMigrateLive initiates QEMU native live migration to a target TCP endpoint.
// The VM keeps running while memory pages are streamed incrementally.
// Returns when migration completes (VM is now running on target).
func qmpMigrateLive(vmDir string, targetIP string, targetPort int) error {
	conn, err := qmpDial(vmDir)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Set migration parameters for faster convergence
	qmpCommand(conn, "migrate-set-capabilities", map[string]interface{}{
		"capabilities": []map[string]interface{}{
			{"capability": "auto-converge", "state": true},
		},
	})

	// Set downtime limit (100ms — QEMU will pause for at most this long)
	qmpCommand(conn, "migrate-set-parameters", map[string]interface{}{
		"downtime-limit": 100,
	})

	// Set max bandwidth (0 = unlimited)
	qmpCommand(conn, "migrate-set-parameters", map[string]interface{}{
		"max-bandwidth": int64(0),
	})

	// Start migration
	migrateURI := fmt.Sprintf("tcp:%s:%d", targetIP, targetPort)
	log.Printf("qmpMigrateLive: starting migration to %s", migrateURI)
	_, err = qmpCommand(conn, "migrate", map[string]interface{}{
		"uri": migrateURI,
	})
	if err != nil {
		return fmt.Errorf("qmp migrate: %w", err)
	}

	// Poll until migration completes. Uses progress-based timeout:
	// keep waiting as long as bytes are being transferred. Only timeout
	// after 2 minutes of zero progress (stall). This handles any VM size
	// without guessing how long the migration will take.
	const stallTimeout = 2 * time.Minute
	log.Printf("qmpMigrateLive: polling with %s stall timeout", stallTimeout)

	lastLog := time.Now()
	lastProgress := time.Now()
	var lastTransferred int64
	startTime := time.Now()

	for {
		time.Sleep(500 * time.Millisecond)

		result, err := qmpCommand(conn, "query-migrate", nil)
		if err != nil {
			if time.Since(lastProgress) > stallTimeout {
				qmpCommand(conn, "migrate_cancel", nil)
				return fmt.Errorf("QEMU live migration stalled (no QMP response for %s)", stallTimeout)
			}
			continue
		}

		var status struct {
			Status    string `json:"status"`
			ErrorDesc string `json:"error-desc"`
			RAM       *struct {
				Transferred int64 `json:"transferred"`
				Remaining   int64 `json:"remaining"`
				Total       int64 `json:"total"`
				Dirty       int64 `json:"dirty-pages-rate"`
				MBps        int64 `json:"mbps"`
			} `json:"ram"`
		}
		json.Unmarshal(result, &status)

		// Track progress
		if status.RAM != nil && status.RAM.Transferred > lastTransferred {
			lastProgress = time.Now()
			lastTransferred = status.RAM.Transferred
		}

		// Log progress every 10s
		if time.Since(lastLog) >= 10*time.Second {
			elapsed := time.Since(startTime).Round(time.Second)
			if status.RAM != nil {
				pct := float64(0)
				if status.RAM.Total > 0 {
					pct = float64(status.RAM.Transferred) / float64(status.RAM.Total) * 100
				}
				log.Printf("qmpMigrateLive: %s — %.1f%% (%dMB/%dMB, %dMB remaining, %d dirty/s, %dMbps) [%s elapsed]",
					status.Status, pct,
					status.RAM.Transferred/1024/1024, status.RAM.Total/1024/1024,
					status.RAM.Remaining/1024/1024, status.RAM.Dirty, status.RAM.MBps, elapsed)
			} else {
				log.Printf("qmpMigrateLive: status=%s [%s elapsed]", status.Status, elapsed)
			}
			lastLog = time.Now()
		}

		switch status.Status {
		case "completed":
			log.Printf("qmpMigrateLive: migration completed in %s", time.Since(startTime).Round(time.Second))
			return nil
		case "failed":
			elapsed := time.Since(startTime).Round(time.Millisecond)
			log.Printf("qmpMigrateLive: FAILED after %s — full query-migrate response: %s", elapsed, string(result))
			if status.ErrorDesc != "" {
				return fmt.Errorf("QEMU live migration failed after %s: %s", elapsed, status.ErrorDesc)
			}
			return fmt.Errorf("QEMU live migration failed after %s (no error-desc in response: %s)", elapsed, string(result))
		case "cancelled":
			return fmt.Errorf("QEMU live migration cancelled")
		}

		// Stall detection
		if time.Since(lastProgress) > stallTimeout {
			qmpCommand(conn, "migrate_cancel", nil)
			return fmt.Errorf("QEMU live migration stalled (no progress for %s, transferred %dMB)",
				stallTimeout, lastTransferred/1024/1024)
		}
	}
}

// qmpLoadState loads a saved state file into a QEMU VM started with -incoming defer.
// ramMiB is used to scale the timeout (large VMs need longer to load state from disk).
func qmpLoadState(vmDir, statePath string, ramMiB ...int) error {
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

	// Poll until loaded. Uses progress-based timeout: keep waiting as long
	// as bytes are being loaded. Only timeout after 2 minutes of zero progress.
	// This handles any VM size without guessing how long the load will take.
	const stallTimeout = 2 * time.Minute
	log.Printf("qmpLoadState: loading %s (stall timeout %s)", statePath, stallTimeout)

	lastQMPLog := time.Now()
	lastProgress := time.Now()
	var lastTransferred int64
	consecutiveErrors := 0
	startTime := time.Now()

	for {
		time.Sleep(500 * time.Millisecond)

		// Check if QEMU process is still alive — detect crashes early
		if !IsRunning(vmDir) {
			return fmt.Errorf("QEMU process died during state load (after %s)",
				time.Since(startTime).Round(time.Second))
		}

		result, err := qmpCommand(conn, "query-migrate", nil)

		// Log QMP response every 30s for diagnostics
		if time.Since(lastQMPLog) >= 30*time.Second {
			elapsed := time.Since(startTime).Round(time.Second)
			if result != nil {
				log.Printf("qmpLoadState: progress at %s: %s", elapsed, string(result))
			} else {
				log.Printf("qmpLoadState: polling at %s (no response)", elapsed)
			}
			lastQMPLog = time.Now()
		}
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= 10 {
				return fmt.Errorf("QEMU appears unresponsive (%d consecutive QMP errors): %w", consecutiveErrors, err)
			}
			// After successful load, query-migrate may return empty
			// Check if VM is running
			statusResult, statusErr := qmpCommand(conn, "query-status", nil)
			if statusErr == nil {
				consecutiveErrors = 0
				var qs struct {
					Status  string `json:"status"`
					Running bool   `json:"running"`
				}
				json.Unmarshal(statusResult, &qs)
				if qs.Status == "postmigrate" || qs.Status == "paused" {
					log.Printf("qmpLoadState: completed in %s", time.Since(startTime).Round(time.Second))
					_, err = qmpCommand(conn, "cont", nil)
					return err
				}
			}
			// If errors but process alive, check stall
			if time.Since(lastProgress) > stallTimeout {
				return fmt.Errorf("qmp state load stalled (no progress for %s, transferred %dMB)",
					stallTimeout, lastTransferred/1024/1024)
			}
			continue
		}
		consecutiveErrors = 0

		// Parse progress for stall detection
		var status struct {
			Status string `json:"status"`
			RAM    *struct {
				Transferred int64 `json:"transferred"`
				Total       int64 `json:"total"`
			} `json:"ram"`
		}
		json.Unmarshal(result, &status)

		if status.RAM != nil && status.RAM.Transferred > lastTransferred {
			lastProgress = time.Now()
			lastTransferred = status.RAM.Transferred
		}

		switch status.Status {
		case "completed":
			log.Printf("qmpLoadState: completed in %s", time.Since(startTime).Round(time.Second))
			_, err = qmpCommand(conn, "cont", nil)
			return err
		case "failed":
			return fmt.Errorf("qmp incoming migration failed")
		}

		// Stall detection
		if time.Since(lastProgress) > stallTimeout {
			return fmt.Errorf("qmp state load stalled (no progress for %s, transferred %dMB)",
				stallTimeout, lastTransferred/1024/1024)
		}
	}
}

// waitForPortListen checks /proc/net/tcp until the given port appears in LISTEN state.
// This avoids making a TCP connection (which QEMU would interpret as a migration stream).
func waitForPortListen(port int, timeout time.Duration) error {
	// In /proc/net/tcp, port is hex in the 2nd column (local_address) after ':'
	// and state 0A = LISTEN.
	portHex := fmt.Sprintf("%04X", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile("/proc/net/tcp")
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			// fields[1] = local_address (hex_ip:hex_port), fields[3] = state
			parts := strings.SplitN(fields[1], ":", 2)
			if len(parts) == 2 && parts[1] == portHex && fields[3] == "0A" {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("port %d not in LISTEN state after %s", port, timeout)
}
