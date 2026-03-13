package vm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// fcClient returns an HTTP client that talks to a Firecracker API socket.
// DisableKeepAlives ensures each request opens and closes its own connection,
// preventing idle connection accumulation that triggers Firecracker's
// "Too many open connections" error during rapid migration retries.
func fcClient(vmDir string) *http.Client {
	return fcClientWithTimeout(vmDir, 2*time.Minute)
}

// fcSnapshotClient returns an HTTP client with a longer timeout for snapshot
// operations. Disk-based snapshots of large VMs under I/O contention can take
// over 2 minutes (Bug #87: 2GB VM snapshot timed out during concurrent drain).
func fcSnapshotClient(vmDir string) *http.Client {
	return fcClientWithTimeout(vmDir, 5*time.Minute)
}

func fcClientWithTimeout(vmDir string, timeout time.Duration) *http.Client {
	sockPath := filepath.Join(vmDir, "api.sock")
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sockPath, 5*time.Second)
			},
		},
	}
}

// fcPause pauses a running Firecracker VM (freezes vCPUs instantly).
func fcPause(vmDir string) error {
	body := map[string]string{"state": "Paused"}
	return fcPatch(vmDir, "/vm", body)
}

// fcResume resumes a paused Firecracker VM.
func fcResume(vmDir string) error {
	body := map[string]string{"state": "Resumed"}
	return fcPatch(vmDir, "/vm", body)
}

// fcSnapshot creates a full snapshot of a paused VM.
// The VM must be paused first. Returns snapshot and memory file paths.
// Writes to /dev/shm (tmpfs) when possible for faster I/O — avoids
// writing multi-GB memory dumps through the QCOW2 virtual disk.
func fcSnapshot(vmDir string) (snapPath, memPath string, err error) {
	vmName := filepath.Base(vmDir)

	// Use a separate "mig" subdirectory to avoid colliding with Firecracker's mmapped
	// vm.mem from a previous snapshot import. Writing to the same path as the mmapped
	// file causes severe slowdown (page faults + COW for every page).
	shmDir := filepath.Join("/dev/shm", "bc-"+vmName+"-mig")
	useSHM := true
	if st, err := LoadVMState(vmDir); err == nil {
		needed := int64(st.RAMMIB) * 1024 * 1024
		if needed > 0 {
			var stat syscall.Statfs_t
			if syscall.Statfs("/dev/shm", &stat) == nil {
				avail := int64(stat.Bavail) * int64(stat.Bsize)
				if avail < needed+needed/5 {
					useSHM = false
					log.Printf("fcSnapshot %s: /dev/shm has %dMB free (need %dMB), using disk",
						vmName, avail/1024/1024, (needed+needed/5)/1024/1024)
				}
			}
		}
	}

	body := map[string]string{
		"snapshot_type": "Full",
	}

	if useSHM {
		os.MkdirAll(shmDir, 0755)
		snapPath = filepath.Join(shmDir, "vm.snap")
		memPath = filepath.Join(shmDir, "vm.mem")
		body["snapshot_path"] = snapPath
		body["mem_file_path"] = memPath
		if err := fcPutWithClient(vmDir, "/snapshot/create", body, fcSnapshotClient(vmDir)); err != nil {
			// Clean up partial files from failed attempt before retrying
			os.RemoveAll(shmDir)
			log.Printf("fcSnapshot %s: /dev/shm failed (%v), falling back to disk", vmName, err)
			useSHM = false
		}
	}

	if !useSHM {
		snapPath = filepath.Join(vmDir, "vm.snap")
		memPath = filepath.Join(vmDir, "vm.mem")
		body["snapshot_path"] = snapPath
		body["mem_file_path"] = memPath
		if err2 := fcPutWithClient(vmDir, "/snapshot/create", body, fcSnapshotClient(vmDir)); err2 != nil {
			return "", "", fmt.Errorf("creating snapshot on disk: %w", err2)
		}
	}
	return snapPath, memPath, nil
}

func fcPatch(vmDir, path string, body interface{}) error {
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PATCH", "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := fcClient(vmDir).Do(req)
	if err != nil {
		return fmt.Errorf("firecracker API %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker API %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}

// fcVsockNudge connects to a guest VM via Firecracker's vsock and sends a
// nudge signal. The guest-side listener triggers network path re-discovery.
func fcVsockNudge(vmDir string, port int) error {
	sockPath := filepath.Join(vmDir, "vsock.sock")
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("vsock dial: %w", err)
	}
	defer conn.Close()

	// Firecracker vsock host-initiated protocol: send "CONNECT <port>\n"
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "CONNECT %d\n", port)

	// Read response — expect "OK <port>\n"
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("vsock read: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("vsock connect rejected: %s", strings.TrimSpace(line))
	}

	return nil
}

// fcPrefaultMemory pre-populates Firecracker's guest memory page tables after
// snapshot restore. Without this, the first snapshot creation after restore takes
// ~25s for a 512MB VM due to 131K lazy page faults. Reading through /proc/<pid>/mem
// forces the kernel to fault in all pages via get_user_pages(), reducing subsequent
// snapshot creation from ~25s to ~260ms.
func fcPrefaultMemory(pid int, vmName string) {
	mapsPath := fmt.Sprintf("/proc/%d/maps", pid)
	mapsData, err := os.ReadFile(mapsPath)
	if err != nil {
		log.Printf("prefault %s: cannot read maps: %v", vmName, err)
		return
	}

	// Collect ALL vm.mem mappings — Firecracker splits guest memory into
	// multiple segments (e.g., 768MB + 3328MB for a 4GB VM). Without
	// faulting all segments, snapshot creation stays slow (Bug #94).
	type memRange struct {
		start, end uint64
	}
	var ranges []memRange
	for _, line := range strings.Split(string(mapsData), "\n") {
		if strings.Contains(line, "vm.mem") {
			parts := strings.SplitN(line, " ", 2)
			addrs := strings.Split(parts[0], "-")
			if len(addrs) == 2 {
				var s, e uint64
				fmt.Sscanf(addrs[0], "%x", &s)
				fmt.Sscanf(addrs[1], "%x", &e)
				if s > 0 && e > s {
					ranges = append(ranges, memRange{s, e})
				}
			}
		}
	}
	if len(ranges) == 0 {
		return // No vm.mem mapping found — fresh VM, not restored
	}

	memPath := fmt.Sprintf("/proc/%d/mem", pid)
	f, err := os.Open(memPath)
	if err != nil {
		log.Printf("prefault %s: cannot open mem: %v", vmName, err)
		return
	}
	defer f.Close()

	var totalSize uint64
	buf := make([]byte, 4*1024*1024) // 4MB chunks
	for _, r := range ranges {
		offset := int64(r.start)
		end := int64(r.end)
		for offset < end {
			n := int64(len(buf))
			if offset+n > end {
				n = end - offset
			}
			f.ReadAt(buf[:n], offset)
			offset += n
		}
		totalSize += r.end - r.start
	}
	log.Printf("prefault %s: pre-faulted %dMB guest memory across %d segments (PID %d)",
		vmName, totalSize/1024/1024, len(ranges), pid)
}

func fcPut(vmDir, path string, body interface{}) error {
	return fcPutWithClient(vmDir, path, body, fcClient(vmDir))
}

func fcPutWithClient(vmDir, path string, body interface{}, client *http.Client) error {
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PUT", "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker API %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker API %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}
