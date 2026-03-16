package vmid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
)

// Client talks to the vmid admin socket.
type Client struct {
	socketPath string
	http       *http.Client
}

func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
		},
	}
}

type RegisterRequest struct {
	VMID        string   `json:"vm_id"`
	VMType      string   `json:"vm_type,omitempty"` // "firecracker" or "qemu"
	IP          string   `json:"ip"`
	Mark        int      `json:"mark"`
	Mode        string   `json:"mode"`
	GitHubRepo  string   `json:"github_repo,omitempty"`
	GitHubRepos []string `json:"github_repos,omitempty"`
}

type ReposResponse struct {
	Repos []string `json:"repos"`
}

func (c *Client) AddRepo(vmID, repo string) (*ReposResponse, error) {
	body, _ := json.Marshal(map[string]string{"repo": repo})
	resp, err := c.http.Post("http://localhost/internal/vms/"+vmID+"/repos", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vmid add repo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vmid add repo: status %d", resp.StatusCode)
	}
	var result ReposResponse
	json.NewDecoder(resp.Body).Decode(&result)
	return &result, nil
}

func (c *Client) RemoveRepo(vmID, repo string) (*ReposResponse, error) {
	req, _ := http.NewRequest("DELETE", "http://localhost/internal/vms/"+vmID+"/repos/"+repo, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vmid remove repo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vmid remove repo: status %d", resp.StatusCode)
	}
	var result ReposResponse
	json.NewDecoder(resp.Body).Decode(&result)
	return &result, nil
}

func (c *Client) ListRepos(vmID string) ([]string, error) {
	resp, err := c.http.Get("http://localhost/internal/vms/" + vmID + "/repos")
	if err != nil {
		return nil, fmt.Errorf("vmid list repos: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vmid list repos: status %d", resp.StatusCode)
	}
	var result ReposResponse
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Repos, nil
}

func (c *Client) Register(req *RegisterRequest) error {
	body, _ := json.Marshal(req)
	resp, err := c.http.Post("http://localhost/internal/vms", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("vmid register: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("vmid register: status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) Deregister(vmID string) error {
	req, _ := http.NewRequest("DELETE", "http://localhost/internal/vms/"+vmID, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vmid deregister: %w", err)
	}
	defer resp.Body.Close()
	return nil
}

type GitHubTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

func (c *Client) MintGitHubToken(vmID string) (*GitHubTokenResponse, error) {
	resp, err := c.http.Post("http://localhost/internal/vms/"+vmID+"/github-token", "", nil)
	if err != nil {
		return nil, fmt.Errorf("vmid github token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("vmid github token: status %d", resp.StatusCode)
	}
	var tok GitHubTokenResponse
	json.NewDecoder(resp.Body).Decode(&tok)
	return &tok, nil
}

// GHCRToken returns a GitHub installation token with packages scope for ghcr.io auth.
func (c *Client) GHCRToken() (string, error) {
	resp, err := c.http.Get("http://localhost/internal/ghcr-token")
	if err != nil {
		return "", fmt.Errorf("vmid ghcr token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("vmid ghcr token: status %d", resp.StatusCode)
	}
	var tok GitHubTokenResponse
	json.NewDecoder(resp.Body).Decode(&tok)
	return tok.Token, nil
}

// ActivityReport mirrors the registry type for JSON decoding.
type ActivityReport struct {
	Timestamp   string `json:"timestamp"`
	PaneContent string `json:"pane_content"`
	Status      string `json:"status"`
	Summary     string `json:"summary,omitempty"`
}

// Message mirrors the registry type for JSON encoding/decoding.
type Message struct {
	ID        string  `json:"id"`
	From      string  `json:"from"`
	Body      string  `json:"body"`
	Priority  string  `json:"priority"`
	SendKeys  bool    `json:"send_keys,omitempty"`
	CreatedAt string  `json:"created_at"`
	ReadAt    *string `json:"read_at,omitempty"`
}

// StatusReport is a self-reported status from Claude Code inside the VM.
type StatusReport struct {
	Timestamp string `json:"timestamp"`
	Status    string `json:"status"`
}

// VMActivitySummary mirrors the registry type.
type VMActivitySummary struct {
	VMID            string          `json:"vm_id"`
	LastActivity    *ActivityReport `json:"last_activity,omitempty"`
	LastStatus      *StatusReport   `json:"last_status,omitempty"`
	PendingMessages int             `json:"pending_messages"`
}

// GetVMActivity returns a VM's latest activity report.
func (c *Client) GetVMActivity(vmID string) (*ActivityReport, error) {
	resp, err := c.http.Get("http://localhost/internal/vms/" + vmID + "/activity")
	if err != nil {
		return nil, fmt.Errorf("vmid get activity: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vmid get activity: status %d", resp.StatusCode)
	}
	var report ActivityReport
	json.NewDecoder(resp.Body).Decode(&report)
	return &report, nil
}

// PostMessage sends a message to a VM's inbox.
func (c *Client) PostMessage(vmID string, msg *Message) error {
	body, _ := json.Marshal(msg)
	resp, err := c.http.Post("http://localhost/internal/vms/"+vmID+"/inbox", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("vmid post message: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("vmid post message: status %d", resp.StatusCode)
	}
	return nil
}

// GetAllActivity returns tapegun activity summaries for all VMs.
func (c *Client) GetAllActivity() ([]VMActivitySummary, error) {
	resp, err := c.http.Get("http://localhost/internal/tapegun/activity")
	if err != nil {
		return nil, fmt.Errorf("vmid all activity: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vmid all activity: status %d", resp.StatusCode)
	}
	var summaries []VMActivitySummary
	json.NewDecoder(resp.Body).Decode(&summaries)
	return summaries, nil
}

func (c *Client) Healthy() bool {
	resp, err := c.http.Get("http://localhost/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}
