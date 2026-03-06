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
	GoldenReady     bool   `json:"golden_ready"`
	Status          string `json:"status"`
}

type ExportResponse struct {
	Name     string          `json:"name"`
	CowPath  string          `json:"cow_path"`
	CowBytes int64           `json:"cow_bytes"`
	VMState  json.RawMessage `json:"vm_state"`
}

// ProgressEvent is a streamed progress line from the node agent.
type ProgressEvent struct {
	Phase       string `json:"phase"`
	Message     string `json:"message,omitempty"`
	Name        string `json:"name,omitempty"`
	TailscaleIP string `json:"tailscale_ip,omitempty"`
	Mark        int    `json:"mark,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Status      string `json:"status,omitempty"`
	Error       string `json:"error,omitempty"`
}

// ProgressFunc is called for each progress event during VM creation.
type ProgressFunc func(event *ProgressEvent)

// CreateStreaming creates a VM and streams progress events via the callback.
// Returns the final CreateResponse.
func (c *Client) CreateStreaming(req *CreateRequest, progress ProgressFunc) (*CreateResponse, error) {
	body, _ := json.Marshal(req)
	resp, err := c.http.Post(c.baseURL+"/api/vms", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("node create: %w", err)
	}
	defer resp.Body.Close()

	// Read NDJSON stream
	decoder := json.NewDecoder(resp.Body)
	var lastEvent ProgressEvent
	for decoder.More() {
		var evt ProgressEvent
		if err := decoder.Decode(&evt); err != nil {
			break
		}
		lastEvent = evt
		if progress != nil && evt.Phase != "ready" && evt.Phase != "error" {
			progress(&evt)
		}
	}

	if lastEvent.Phase == "error" {
		return nil, fmt.Errorf("node create: %s", lastEvent.Error)
	}

	if lastEvent.Phase == "ready" {
		if progress != nil {
			progress(&lastEvent)
		}
		return &CreateResponse{
			Name:        lastEvent.Name,
			TailscaleIP: lastEvent.TailscaleIP,
			Mark:        lastEvent.Mark,
			Mode:        lastEvent.Mode,
			Status:      lastEvent.Status,
		}, nil
	}

	return nil, fmt.Errorf("node create: unexpected end of stream")
}

func (c *Client) Create(req *CreateRequest) (*CreateResponse, error) {
	return c.CreateStreaming(req, nil)
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

// BuildGolden triggers a golden image build on the node, streaming progress.
func (c *Client) BuildGolden(progress func(phase, message string)) error {
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Post(c.baseURL+"/api/golden/build", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	for decoder.More() {
		var evt ProgressEvent
		if err := decoder.Decode(&evt); err != nil {
			break
		}
		if evt.Phase == "error" {
			return fmt.Errorf("%s", evt.Message)
		}
		if progress != nil {
			progress(evt.Phase, evt.Message)
		}
	}
	return nil
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

type MigrateRequest struct {
	TargetAddr     string `json:"target_addr"`
	TargetBridgeIP string `json:"target_bridge_ip"`
}

type MigrateResponse struct {
	Name        string `json:"name"`
	TailscaleIP string `json:"tailscale_ip,omitempty"`
	Mark        int    `json:"mark"`
	TargetNode  string `json:"target_node"`
	Status      string `json:"status"`
}

func (c *Client) Migrate(name string, req *MigrateRequest) (*MigrateResponse, error) {
	body, _ := json.Marshal(req)
	resp, err := c.http.Post(c.baseURL+"/api/vms/"+name+"/migrate", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("migrate request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("migrate: %s", string(errBody))
	}
	var mr MigrateResponse
	json.NewDecoder(resp.Body).Decode(&mr)
	return &mr, nil
}

// VMDetail is the full VM info returned by the node agent.
type VMDetail struct {
	Name        string `json:"name"`
	TailscaleIP string `json:"tailscale_ip"`
	Mark        int    `json:"mark"`
	Mode        string `json:"mode"`
	VCPU        int    `json:"vcpu"`
	RAMMIB      int    `json:"ram_mib"`
	Disk        string `json:"disk"`
	Status      string `json:"status"`
}

// FastClient is a node client with a short timeout for non-critical queries.
type FastClient struct {
	baseURL string
	http    *http.Client
}

func NewFastClient(apiAddr string) *FastClient {
	return &FastClient{
		baseURL: "http://" + apiAddr,
		http:    &http.Client{Timeout: 2 * time.Second},
	}
}

// GetVM fetches detail for a single VM from the node. Returns nil on any error.
func (c *FastClient) GetVM(name string) *VMDetail {
	resp, err := c.http.Get(c.baseURL + "/api/vms/" + name)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil
	}
	// Node returns {"vm": {...}, "status": "..."}
	var result struct {
		VM     VMDetail `json:"vm"`
		Status string   `json:"status"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return nil
	}
	result.VM.Status = result.Status
	return &result.VM
}

// ListVMs fetches all VMs from the node. Returns nil on any error.
func (c *FastClient) ListVMs() []VMDetail {
	resp, err := c.http.Get(c.baseURL + "/api/vms")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil
	}
	var vms []VMDetail
	json.NewDecoder(resp.Body).Decode(&vms)
	return vms
}

// Health fetches health from the node. Returns nil on any error.
func (c *FastClient) Health() *HealthResponse {
	resp, err := c.http.Get(c.baseURL + "/api/health")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var hr HealthResponse
	if json.NewDecoder(resp.Body).Decode(&hr) != nil {
		return nil
	}
	return &hr
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
