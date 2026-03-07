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
	VMID       string `json:"vm_id"`
	IP         string `json:"ip"`
	Mark       int    `json:"mark"`
	Mode       string `json:"mode"`
	GitHubRepo string `json:"github_repo,omitempty"`
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

func (c *Client) Healthy() bool {
	resp, err := c.http.Get("http://localhost/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}
