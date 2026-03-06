package vm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// fcClient returns an HTTP client that talks to a Firecracker API socket.
func fcClient(vmDir string) *http.Client {
	sockPath := filepath.Join(vmDir, "api.sock")
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
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
func fcSnapshot(vmDir string) (snapPath, memPath string, err error) {
	snapPath = filepath.Join(vmDir, "vm.snap")
	memPath = filepath.Join(vmDir, "vm.mem")

	body := map[string]string{
		"snapshot_type": "Full",
		"snapshot_path": snapPath,
		"mem_file_path": memPath,
	}
	if err := fcPut(vmDir, "/snapshot/create", body); err != nil {
		return "", "", fmt.Errorf("creating snapshot: %w", err)
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

func fcPut(vmDir, path string, body interface{}) error {
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PUT", "http://localhost"+path, bytes.NewReader(data))
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
