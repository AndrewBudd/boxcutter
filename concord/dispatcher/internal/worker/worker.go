package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AndrewBudd/boxcutter/concord/dispatcher/internal/task"
)

// Client manages a worker VM's lifecycle through the boxcutter orchestrator API.
type Client struct {
	orchestratorURL string
	http            *http.Client
}

// NewClient creates a worker client that talks to the boxcutter orchestrator.
func NewClient(orchestratorURL string) *Client {
	return &Client{
		orchestratorURL: strings.TrimRight(orchestratorURL, "/"),
		http:            &http.Client{Timeout: 5 * time.Minute},
	}
}

// CreateVM creates a new worker VM. Returns the VM name.
func (c *Client) CreateVM(name string, ramMIB int) (string, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"name":    name,
		"ram_mib": ramMIB,
	})
	resp, err := c.http.Post(c.orchestratorURL+"/api/vms", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create VM: %w", err)
	}
	defer resp.Body.Close()

	// Read NDJSON stream for progress events until "ready"
	decoder := json.NewDecoder(resp.Body)
	var result struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Phase  string `json:"phase"`
		Error  string `json:"error"`
	}
	for decoder.More() {
		if err := decoder.Decode(&result); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}
		if result.Phase == "ready" || result.Status == "running" {
			return result.Name, nil
		}
		if result.Error != "" {
			return "", fmt.Errorf("create VM: %s", result.Error)
		}
	}

	if result.Name != "" {
		return result.Name, nil
	}
	return "", fmt.Errorf("create VM: no ready event received")
}

// DestroyVM destroys a worker VM.
func (c *Client) DestroyVM(name string) error {
	req, _ := http.NewRequest("DELETE", c.orchestratorURL+"/api/vms/"+name, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("destroy VM: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("destroy VM: status %d", resp.StatusCode)
	}
	return nil
}

// DeployAgent copies the install script to the VM and runs it.
func (c *Client) DeployAgent(vmName string, installScript []byte) error {
	// Copy install script to VM
	if err := c.CopyTo(vmName, "/tmp/concord-install.sh", bytes.NewReader(installScript)); err != nil {
		return fmt.Errorf("copy agent installer: %w", err)
	}
	// Run the installer
	result, err := c.Exec(vmName, "bash /tmp/concord-install.sh && rm /tmp/concord-install.sh")
	if err != nil {
		return fmt.Errorf("run agent installer: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("agent install failed (exit %d): %s", result.ExitCode, result.Output)
	}
	return nil
}

// SubmitTask sends a task payload to the worker VM and triggers execution.
func (c *Client) SubmitTask(vmName string, t *task.Task) error {
	payload, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}
	// Copy task payload to inbox
	if err := c.CopyTo(vmName, "/opt/concord-agent/inbox/task.json", bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("copy task payload: %w", err)
	}
	// Trigger execution
	result, err := c.Exec(vmName, "/opt/concord-agent/run-task.sh")
	if err != nil {
		return fmt.Errorf("trigger task: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("task execution failed (exit %d): %s", result.ExitCode, result.Output)
	}
	return nil
}

// PollStatus retrieves the task status from the worker VM.
func (c *Client) PollStatus(vmName string) (*task.Status, error) {
	result, err := c.Exec(vmName, "cat /opt/concord-agent/status.json")
	if err != nil {
		return nil, fmt.Errorf("poll status: %w", err)
	}
	var status task.Status
	if err := json.Unmarshal([]byte(result.Output), &status); err != nil {
		return nil, fmt.Errorf("parse status: %w (output: %s)", err, result.Output)
	}
	return &status, nil
}

// WaitForCompletion polls the worker until the task reaches a terminal state.
func (c *Client) WaitForCompletion(vmName string, pollInterval time.Duration, timeout time.Duration) (*task.Status, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := c.PollStatus(vmName)
		if err != nil {
			// Transient errors — keep polling
			time.Sleep(pollInterval)
			continue
		}
		if status.IsTerminal() {
			return status, nil
		}
		time.Sleep(pollInterval)
	}
	return nil, fmt.Errorf("task timed out after %s", timeout)
}

// GetLogs retrieves task execution logs from the worker VM.
func (c *Client) GetLogs(vmName string) (string, error) {
	result, err := c.Exec(vmName, "cat /opt/concord-agent/logs/task.log 2>/dev/null || echo '(no logs)'")
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

// RetrieveOutput copies output artifacts from the worker VM.
func (c *Client) RetrieveOutput(vmName, remotePath string, dst io.Writer) error {
	return c.CopyFrom(vmName, remotePath, dst)
}

// Exec runs a command on the worker VM and returns the output.
func (c *Client) Exec(vmName, command string) (*ExecResult, error) {
	body, _ := json.Marshal(map[string]string{"command": command})
	resp, err := c.http.Post(c.orchestratorURL+"/api/vms/"+vmName+"/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("exec: %s", string(respBody))
	}
	var result ExecResult
	json.Unmarshal(respBody, &result)
	return &result, nil
}

// CopyTo copies data to a file on the worker VM.
func (c *Client) CopyTo(vmName, remotePath string, src io.Reader) error {
	url := fmt.Sprintf("%s/api/vms/%s/cp-to?path=%s", c.orchestratorURL, vmName, remotePath)
	resp, err := c.http.Post(url, "application/octet-stream", src)
	if err != nil {
		return fmt.Errorf("cp-to: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cp-to: %s", string(body))
	}
	return nil
}

// CopyFrom copies a file from the worker VM.
func (c *Client) CopyFrom(vmName, remotePath string, dst io.Writer) error {
	url := fmt.Sprintf("%s/api/vms/%s/cp-from?path=%s", c.orchestratorURL, vmName, remotePath)
	resp, err := c.http.Get(url)
	if err != nil {
		return fmt.Errorf("cp-from: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cp-from: %s", string(body))
	}
	_, err = io.Copy(dst, resp.Body)
	return err
}

// ExecResult is the output of an exec command.
type ExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}
