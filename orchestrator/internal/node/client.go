package node

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to a node agent's HTTP API.
type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(apiAddr string) *Client {
	return &Client{
		baseURL: "http://" + apiAddr,
		http: &http.Client{
			Timeout: 5 * time.Minute, // VM operations can be slow
		},
	}
}

type CreateRequest struct {
	Name           string   `json:"name"`
	VCPU           int      `json:"vcpu,omitempty"`
	RAMMIB         int      `json:"ram_mib,omitempty"`
	Disk           string   `json:"disk,omitempty"`
	CloneURL       string   `json:"clone_url,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	AuthorizedKeys []string `json:"authorized_keys,omitempty"`
}

type CreateResponse struct {
	Name        string `json:"name"`
	TailscaleIP string `json:"tailscale_ip"`
	Mark        int    `json:"mark"`
	Mode        string `json:"mode"`
	Status      string `json:"status"`
}

type HealthResponse struct {
	Hostname        string `json:"hostname"`
	VCPUTotal       int    `json:"vcpu_total"`
	RAMTotalMIB     int    `json:"ram_total_mib"`
	RAMAllocatedMIB int    `json:"ram_allocated_mib"`
	RAMFreeMIB      int    `json:"ram_free_mib"`
	VMsTotal        int    `json:"vms_total"`
	VMsRunning      int    `json:"vms_running"`
	Status          string `json:"status"`
}

type ExportResponse struct {
	Name     string          `json:"name"`
	CowPath  string          `json:"cow_path"`
	CowBytes int64           `json:"cow_bytes"`
	VMState  json.RawMessage `json:"vm_state"`
}

func (c *Client) Create(req *CreateRequest) (*CreateResponse, error) {
	body, _ := json.Marshal(req)
	resp, err := c.http.Post(c.baseURL+"/api/vms", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("node create: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("node create: %s (status %d)", string(errBody), resp.StatusCode)
	}

	var cr CreateResponse
	json.NewDecoder(resp.Body).Decode(&cr)
	return &cr, nil
}

func (c *Client) Destroy(name string) error {
	req, _ := http.NewRequest("DELETE", c.baseURL+"/api/vms/"+name, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("node destroy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("node destroy: %s", string(errBody))
	}
	return nil
}

func (c *Client) Stop(name string) error {
	resp, err := c.http.Post(c.baseURL+"/api/vms/"+name+"/stop", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("node stop: %s", string(errBody))
	}
	return nil
}

func (c *Client) Start(name string) (*CreateResponse, error) {
	resp, err := c.http.Post(c.baseURL+"/api/vms/"+name+"/start", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("node start: %s", string(errBody))
	}
	var cr CreateResponse
	json.NewDecoder(resp.Body).Decode(&cr)
	return &cr, nil
}

func (c *Client) Health() (*HealthResponse, error) {
	resp, err := c.http.Get(c.baseURL + "/api/health")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var hr HealthResponse
	json.NewDecoder(resp.Body).Decode(&hr)
	return &hr, nil
}

func (c *Client) Export(name string) (*ExportResponse, error) {
	resp, err := c.http.Post(c.baseURL+"/api/vms/"+name+"/export", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("export: %s", string(errBody))
	}
	var er ExportResponse
	json.NewDecoder(resp.Body).Decode(&er)
	return &er, nil
}

func (c *Client) Import(name string, vmState json.RawMessage) (*CreateResponse, error) {
	resp, err := c.http.Post(c.baseURL+"/api/vms/"+name+"/import", "application/json", bytes.NewReader(vmState))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("import: %s", string(errBody))
	}
	var cr CreateResponse
	json.NewDecoder(resp.Body).Decode(&cr)
	return &cr, nil
}

func (c *Client) ListVMs() ([]json.RawMessage, error) {
	resp, err := c.http.Get(c.baseURL + "/api/vms")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var vms []json.RawMessage
	json.NewDecoder(resp.Body).Decode(&vms)
	return vms, nil
}
