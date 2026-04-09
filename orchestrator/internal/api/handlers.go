package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/collab"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
	orchmqtt "github.com/AndrewBudd/boxcutter/orchestrator/internal/mqtt"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/node"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/queue"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/scheduler"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/state"
)

// queuedMessage is a tapegun message waiting for delivery to a migrating VM.
type queuedMessage struct {
	vmName   string
	msg      node.TapegunMessage
	queuedAt time.Time
}

// AlertEvent records a state transition for a VM.
type AlertEvent struct {
	Timestamp string `json:"timestamp"`
	VMName    string `json:"name"`
	Type      string `json:"type"`    // crash, idle, degraded, unhealthy, unresponsive, error, recovery
	Message   string `json:"message"`
	Team      string `json:"team,omitempty"`
}

type Handler struct {
	db    *db.DB
	state *state.Store
	mqtt  *orchmqtt.Client

	// Tapegun message queue: messages for VMs that are temporarily unreachable
	// (e.g., during migration). Background goroutine retries delivery.
	msgQueue   []queuedMessage
	msgQueueMu sync.Mutex

	// MaxVMSizeMB is the largest VM (by RAM in MiB) the cluster can place.
	// VM creation requests exceeding this are rejected.
	MaxVMSizeMB int

	// Work queue: auto-assigns GitHub issues to idle workers
	workQueue  *queue.Queue
	dispatcher *queue.Dispatcher

	// Alert state: previous activity status per VM for transition detection
	prevStatus   map[string]string // vmName → last known activity status
	prevStatusMu sync.RWMutex
	prevHealth   map[string]string // vmName → last known health
	lastReport   map[string]time.Time // vmName → last activity timestamp

	// Alert ring buffer (last 200 events)
	alerts   []AlertEvent
	alertsMu sync.RWMutex

	// SSE subscribers
	sseClients   map[chan AlertEvent]bool
	sseClientsMu sync.Mutex

	// Token usage metrics per VM
	vmMetrics   map[string]*VMMetrics
	vmMetricsMu sync.RWMutex

	// Cross-agent collaboration hub
	collabHub *collab.Hub
}

// VMMetrics stores token usage and utilization data for a single VM.
type VMMetrics struct {
	VMName           string  `json:"name"`
	Team             string  `json:"team,omitempty"`
	APICalls         int     `json:"api_calls"`
	InputTokens      int     `json:"input_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheCreateTokens int    `json:"cache_create_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	Model            string  `json:"model"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
	LastUpdated      string  `json:"last_updated,omitempty"`
}

type modelPricing struct {
	InputPerM      float64
	OutputPerM     float64
	CacheReadPerM  float64
	CacheWritePerM float64
}

var pricing = map[string]modelPricing{
	"claude-opus-4-6":   {15.0, 75.0, 1.5, 15.0},
	"claude-sonnet-4-6": {3.0, 15.0, 0.3, 3.0},
	"claude-haiku-4-5":  {0.80, 4.0, 0.08, 0.80},
}

func estimateCost(m *VMMetrics) float64 {
	p, ok := pricing[m.Model]
	if !ok {
		p = pricing["claude-sonnet-4-6"] // default
	}
	return (float64(m.InputTokens)*p.InputPerM +
		float64(m.OutputTokens)*p.OutputPerM +
		float64(m.CacheReadTokens)*p.CacheReadPerM +
		float64(m.CacheCreateTokens)*p.CacheWritePerM) / 1e6
}

func NewHandler(database *db.DB, store *state.Store) *Handler {
	maxVM := 0
	if v := os.Getenv("MAX_VM_SIZE"); v != "" {
		fmt.Sscanf(v, "%d", &maxVM)
	}
	h := &Handler{
		db:          database,
		state:       store,
		MaxVMSizeMB: maxVM,
		prevStatus:  make(map[string]string),
		prevHealth:  make(map[string]string),
		lastReport:  make(map[string]time.Time),
		sseClients:  make(map[chan AlertEvent]bool),
		vmMetrics:   make(map[string]*VMMetrics),
	}
	h.collabHub = collab.NewHub()
	h.discoverNodes()
	go h.healthMonitorLoop()
	go h.messageDeliveryLoop()
	h.initWorkQueue()
	go h.alertDetectionLoop()
	go h.conflictDetectionLoop()
	return h
}

// discoverNodes scans the bridge subnet for responsive node agents and
// registers them in the in-memory store. This allows the orchestrator to
// rebuild its state from scratch on startup without any persistent state.
func (h *Handler) discoverNodes() {
	subnet := os.Getenv("BRIDGE_SUBNET")
	if subnet == "" {
		subnet = "192.168.50"
	}

	log.Printf("Discovering nodes on %s.0/24 ...", subnet)

	type result struct {
		ip     string
		health *node.HealthResponse
	}

	var mu sync.Mutex
	var results []result
	var wg sync.WaitGroup

	// Scan .3 through .254 (skip .1 = host, .2 = orchestrator)
	for i := 3; i <= 254; i++ {
		ip := fmt.Sprintf("%s.%d", subnet, i)
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			fc := node.NewFastClient(ip + ":8800")
			health := fc.Health()
			if health == nil {
				return
			}
			mu.Lock()
			results = append(results, result{ip: ip, health: health})
			mu.Unlock()
		}(ip)
	}
	wg.Wait()

	if len(results) == 0 {
		log.Printf("Node discovery: no nodes found on subnet %s.0/24", subnet)
		return
	}

	now := time.Now().Format(time.RFC3339)
	for _, r := range results {
		apiAddr := r.ip + ":8800"
		nodeID := r.health.Hostname
		if nodeID == "" {
			nodeID = r.ip
		}

		n := &state.Node{
			ID:            nodeID,
			TailscaleName: nodeID,
			BridgeIP:      r.ip,
			APIAddr:       apiAddr,
			Status:        "active",
			DiscoveredAt:  now,
			LastSeen:      now,
		}
		h.state.SetNode(n)
		log.Printf("Node discovery: registered %s at %s (RAM %d/%d MiB, %d VMs)",
			nodeID, r.ip, r.health.RAMAllocatedMIB, r.health.RAMTotalMIB, r.health.VMsRunning)

		// Sync VM inventory from node
		fc := node.NewFastClient(apiAddr)
		if vmList := fc.ListVMs(); vmList != nil {
			var stateVMs []state.VM
			for _, v := range vmList {
				stateVMs = append(stateVMs, state.VM{
					Name:   v.Name,
					NodeID: nodeID,
					Status: v.Status,
				})
			}
			h.state.SyncNodeVMs(nodeID, stateVMs)
			log.Printf("Node discovery: synced %d VMs from %s", len(stateVMs), nodeID)
		}

		// Sync golden image versions
		if versions := fc.GoldenVersions(); versions != nil {
			h.state.SetGoldenImages(nodeID, versions)
		}
	}

	log.Printf("Node discovery complete: found %d node(s)", len(results))
}

// SetMQTT sets the MQTT client for publishing golden head updates.
func (h *Handler) SetMQTT(mc *orchmqtt.Client) {
	h.mqtt = mc

	// On connect, publish the current golden head (if any)
	if mc != nil {
		go func() {
			head := h.db.GetGoldenHead()
			if head != "" {
				if err := mc.PublishGoldenHead(head); err != nil {
					log.Printf("mqtt: failed to publish initial golden head: %v", err)
				}
			}
		}()
	}
}

// healthMonitorLoop checks node health every 30 seconds, marks nodes down/up,
// and syncs VM inventory and golden image data from each active node.
func (h *Handler) healthMonitorLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		nodes := h.state.ListNodes()
		for _, n := range nodes {
			if n.Status != "active" && n.Status != "down" {
				continue
			}
			fc := node.NewFastClient(n.APIAddr)
			health := fc.Health()
			if health == nil {
				if n.Status == "active" {
					log.Printf("Node %s: unreachable, marking down", n.ID)
					h.state.SetNodeStatus(n.ID, "down")
				}
				continue
			}
			if n.Status == "down" {
				log.Printf("Node %s: back up, marking active", n.ID)
				h.state.SetNodeStatus(n.ID, "active")
			}
			h.state.UpdateNodeLastSeen(n.ID)

			// Sync VM inventory from node
			if vmList := fc.ListVMs(); vmList != nil {
				var stateVMs []state.VM
				for _, v := range vmList {
					stateVMs = append(stateVMs, state.VM{
						Name:   v.Name,
						NodeID: n.ID,
						Status: v.Status,
					})
				}
				h.state.SyncNodeVMs(n.ID, stateVMs)
			}

			// Sync golden image versions from node
			if versions := fc.GoldenVersions(); versions != nil {
				h.state.SetGoldenImages(n.ID, versions)
			}
		}
	}
}

func (h *Handler) Register(mux *http.ServeMux) {
	// Node management
	mux.HandleFunc("POST /api/nodes/register", h.handleNodeRegister)
	mux.HandleFunc("POST /api/nodes/{id}/heartbeat", h.handleNodeHeartbeat)
	mux.HandleFunc("GET /api/nodes", h.handleNodeList)
	mux.HandleFunc("GET /api/nodes/{id}", h.handleNodeGet)

	// VM management
	mux.HandleFunc("POST /api/vms", h.handleVMCreate)
	mux.HandleFunc("GET /api/vms", h.handleVMList)
	mux.HandleFunc("GET /api/vms/{name}", h.handleVMGet)
	mux.HandleFunc("GET /api/vms/{name}/logs", h.handleVMLogs)
	mux.HandleFunc("PATCH /api/vms/{name}", h.handleVMUpdate)
	mux.HandleFunc("DELETE /api/vms/{name}", h.handleVMDestroy)
	mux.HandleFunc("POST /api/vms/{name}/stop", h.handleVMStop)
	mux.HandleFunc("POST /api/vms/{name}/start", h.handleVMStart)
	mux.HandleFunc("POST /api/vms/{name}/copy", h.handleVMCopy)
	mux.HandleFunc("POST /api/vms/{name}/repos", h.handleVMAddRepo)
	mux.HandleFunc("DELETE /api/vms/{name}/repos/{repo...}", h.handleVMRemoveRepo)
	mux.HandleFunc("GET /api/vms/{name}/repos", h.handleVMListRepos)
	mux.HandleFunc("POST /api/vms/{name}/projects", h.handleVMAddProject)
	mux.HandleFunc("DELETE /api/vms/{name}/projects/{project...}", h.handleVMRemoveProject)
	mux.HandleFunc("GET /api/vms/{name}/projects", h.handleVMListProjects)

	// Golden images
	mux.HandleFunc("GET /api/golden", h.handleGoldenList)
	mux.HandleFunc("GET /api/golden/head", h.handleGoldenGetHead)
	mux.HandleFunc("POST /api/golden/head", h.handleGoldenSetHead)

	// SSH keys
	mux.HandleFunc("POST /api/keys/add", h.handleAddKeys)
	mux.HandleFunc("GET /api/keys", h.handleListKeys)
	mux.HandleFunc("DELETE /api/keys/{user}", h.handleDeleteKeys)

	// Health
	mux.HandleFunc("GET /api/health", h.handleHealth)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	// Tapegun
	mux.HandleFunc("GET /api/tapegun/activity", h.handleTapegunActivity)
	mux.HandleFunc("GET /api/tapegun/activity/{name}", h.handleTapegunVMActivity)
	mux.HandleFunc("POST /api/tapegun/message/{name}", h.handleTapegunMessage)
	mux.HandleFunc("POST /api/tapegun/broadcast", h.handleTapegunBroadcast)

	// Dashboard: SSE stream + alerts
	mux.HandleFunc("GET /api/alerts", h.handleAlerts)
	mux.HandleFunc("GET /api/alerts/stream", h.handleAlertStream)

	// Metrics: token usage and cost
	mux.HandleFunc("GET /api/metrics", h.handleMetrics)
	mux.HandleFunc("POST /api/metrics/{name}", h.handleMetricsUpdate)

	// VM exec
	mux.HandleFunc("POST /api/vms/{name}/exec", h.handleVMExec)
	mux.HandleFunc("POST /api/vms/{name}/cp-to", h.handleVMCopyTo)
	mux.HandleFunc("GET /api/vms/{name}/cp-from", h.handleVMCopyFrom)

	// VM-to-VM messaging: lookup which node a VM is on
	mux.HandleFunc("GET /api/vms/{name}/location", h.handleVMLocation)

	// Dashboard: SSE stream + alerts
	mux.HandleFunc("GET /api/alerts", h.handleAlerts)
	mux.HandleFunc("GET /api/alerts/stream", h.handleAlertStream)

	// Team collaboration
	mux.HandleFunc("POST /api/team/{team}/message", h.handleTeamMessage)
	mux.HandleFunc("GET /api/team/{team}/messages", h.handleTeamMessages)
	mux.HandleFunc("POST /api/team/{team}/files", h.handleTeamReportFiles)
	mux.HandleFunc("GET /api/team/{team}/files", h.handleTeamGetFiles)
	mux.HandleFunc("GET /api/team/{team}/conflicts", h.handleTeamConflicts)
	mux.HandleFunc("POST /api/team/{team}/scratchpad", h.handleTeamScratchpadAppend)
	mux.HandleFunc("GET /api/team/{team}/scratchpad", h.handleTeamScratchpadGet)

	// Work queue
	mux.HandleFunc("GET /api/queue", h.handleQueueList)
	mux.HandleFunc("POST /api/queue", h.handleQueueAdd)
	mux.HandleFunc("GET /api/queue/stats", h.handleQueueStats)
	mux.HandleFunc("POST /api/queue/{id}/complete", h.handleQueueComplete)
	mux.HandleFunc("POST /api/queue/{id}/reassign", h.handleQueueReassign)
	mux.HandleFunc("POST /api/queue/sync", h.handleQueueSync)
}

// --- Node handlers ---

func (h *Handler) handleNodeRegister(w http.ResponseWriter, r *http.Request) {
	var n state.Node
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if n.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if n.Status == "" {
		n.Status = "active"
	}
	now := time.Now().Format(time.RFC3339)
	if n.DiscoveredAt == "" {
		n.DiscoveredAt = now
	}
	n.LastSeen = now

	h.state.SetNode(&n)

	log.Printf("Node registered: %s (%s)", n.ID, n.TailscaleName)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, &n)
}

func (h *Handler) handleNodeHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathSegment(r.URL.Path, "/api/nodes/", "/heartbeat")
	}

	h.state.UpdateNodeLastSeen(id)
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handler) handleNodeList(w http.ResponseWriter, r *http.Request) {
	nodes := h.state.ListNodes()

	// Enrich with real-time health from each node (fast timeout, parallel)
	var wg sync.WaitGroup
	for i, n := range nodes {
		if n.APIAddr == "" || n.Status != "active" {
			continue
		}
		wg.Add(1)
		go func(idx int, apiAddr string) {
			defer wg.Done()
			fc := node.NewFastClient(apiAddr)
			if health := fc.Health(); health != nil {
				nodes[idx].RAMTotalMIB = health.RAMTotalMIB
				nodes[idx].RAMAllocatedMIB = health.RAMAllocatedMIB
				nodes[idx].RAMAvailableMIB = health.RAMAvailableMIB
				nodes[idx].DiskTotalMB = health.DiskTotalMB
				nodes[idx].DiskUsedMB = health.DiskUsedMB
				nodes[idx].DiskVMsMB = health.DiskVMsMB
				nodes[idx].VMsRunning = health.VMsRunning
			}
		}(i, n.APIAddr)
	}
	wg.Wait()

	writeJSON(w, nodes)
}

func (h *Handler) handleNodeGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractName(r.URL.Path, "/api/nodes/")
	}
	n := h.state.GetNode(id)
	if n == nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, n)
}

// --- VM handlers ---

type vmCreateRequest struct {
	Name             string            `json:"name"`
	Type             string            `json:"type,omitempty"`
	Description      string            `json:"description,omitempty"`
	VCPU             int               `json:"vcpu,omitempty"`
	RAMMIB           int               `json:"ram_mib,omitempty"`
	Disk             string            `json:"disk,omitempty"`
	CloneURL         string            `json:"clone_url,omitempty"`
	CloneURLs        []string          `json:"clone_urls,omitempty"`
	Mode             string            `json:"mode,omitempty"`
	NodeID           string            `json:"node_id,omitempty"`
	TailscaleAuthkey string            `json:"tailscale_authkey,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
}

// nodeToScheduler converts a state.Node to the db.Node type used by the scheduler.
func nodeToScheduler(n *state.Node) *db.Node {
	return &db.Node{
		ID:              n.ID,
		TailscaleName:   n.TailscaleName,
		TailscaleIP:     n.TailscaleIP,
		BridgeIP:        n.BridgeIP,
		APIAddr:         n.APIAddr,
		Status:          n.Status,
		RAMTotalMIB:     n.RAMTotalMIB,
		RAMAllocatedMIB: n.RAMAllocatedMIB,
		RAMAvailableMIB: n.RAMAvailableMIB,
		DiskTotalMB:     n.DiskTotalMB,
		DiskUsedMB:      n.DiskUsedMB,
		DiskVMsMB:       n.DiskVMsMB,
		VMsRunning:      n.VMsRunning,
	}
}

func (h *Handler) handleVMCreate(w http.ResponseWriter, r *http.Request) {
	var req vmCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		out, err := exec.Command("boxcutter-names", "generate").Output()
		if err != nil {
			http.Error(w, "failed to generate name", http.StatusInternalServerError)
			return
		}
		req.Name = strings.TrimSpace(string(out))
	}
	if req.VCPU == 0 {
		req.VCPU = 2
	}
	if req.RAMMIB == 0 {
		req.RAMMIB = 2048
	}
	if req.Disk == "" {
		req.Disk = "50G"
	}

	// Reject VMs larger than the cluster's max VM size
	if h.MaxVMSizeMB > 0 && req.RAMMIB > h.MaxVMSizeMB {
		http.Error(w, fmt.Sprintf("requested RAM %d MiB exceeds max VM size %d MiB", req.RAMMIB, h.MaxVMSizeMB), http.StatusBadRequest)
		return
	}

	// Check if VM already exists
	if v := h.state.GetVM(req.Name); v != nil {
		http.Error(w, fmt.Sprintf("VM '%s' already exists", req.Name), http.StatusConflict)
		return
	}

	// Get current SSH keys to pass to node
	sshKeys, _ := h.db.ListSSHKeys()

	// Build candidate node list
	var candidates []*db.Node
	if req.NodeID != "" {
		targetNode := h.state.GetNode(req.NodeID)
		if targetNode == nil {
			http.Error(w, "specified node not found", http.StatusBadRequest)
			return
		}
		candidates = []*db.Node{nodeToScheduler(targetNode)}
	} else {
		activeNodes := h.state.ActiveNodes()
		if len(activeNodes) == 0 {
			http.Error(w, "no active nodes", http.StatusServiceUnavailable)
			return
		}
		// Query real-time health and filter reachable nodes
		for _, n := range activeNodes {
			fc := node.NewFastClient(n.APIAddr)
			if health := fc.Health(); health != nil {
				if !health.GoldenReady {
					log.Printf("Scheduling: node %s has no golden image, skipping", n.ID)
					continue
				}
				sn := nodeToScheduler(n)
				sn.RAMAllocatedMIB = health.RAMAllocatedMIB
				sn.RAMAvailableMIB = health.RAMAvailableMIB
				sn.VMsRunning = health.VMsRunning
				sn.RAMTotalMIB = health.RAMTotalMIB
				candidates = append(candidates, sn)
			} else {
				log.Printf("Scheduling: node %s unreachable, skipping", n.ID)
			}
		}
		if len(candidates) == 0 {
			http.Error(w, "no reachable nodes", http.StatusServiceUnavailable)
			return
		}
		sorted, err := scheduler.PickNode(candidates, req.RAMMIB)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		var reordered []*db.Node
		reordered = append(reordered, sorted)
		for _, c := range candidates {
			if c.ID != sorted.ID {
				reordered = append(reordered, c)
			}
		}
		candidates = reordered
	}

	// Set up streaming response
	flusher, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusCreated)

	emitProgress := func(phase, message string) {
		line, _ := json.Marshal(map[string]string{"phase": phase, "message": message})
		fmt.Fprintf(w, "%s\n", line)
		if canFlush {
			flusher.Flush()
		}
	}

	// Try each candidate node until one succeeds
	var nodeResp *node.CreateResponse
	var targetNode *db.Node
	for _, candidate := range candidates {
		emitProgress("scheduling", fmt.Sprintf("Trying %s...", candidate.TailscaleName))

		client := node.NewClient(candidate.APIAddr)
		resp, err := client.CreateStreaming(&node.CreateRequest{
			Name:             req.Name,
			Type:             req.Type,
			Description:      req.Description,
			VCPU:             req.VCPU,
			RAMMIB:           req.RAMMIB,
			Disk:             req.Disk,
			CloneURL:         req.CloneURL,
			CloneURLs:        req.CloneURLs,
			Mode:             req.Mode,
			AuthorizedKeys:   sshKeys,
			TailscaleAuthkey: req.TailscaleAuthkey,
		}, func(evt *node.ProgressEvent) {
			line, _ := json.Marshal(evt)
			fmt.Fprintf(w, "%s\n", line)
			if canFlush {
				flusher.Flush()
			}
		})
		if err != nil {
			log.Printf("Create on %s failed: %v", candidate.ID, err)
			emitProgress("retry", fmt.Sprintf("%s failed: %v", candidate.TailscaleName, err))
			h.state.SetNodeStatus(candidate.ID, "down")
			continue
		}
		nodeResp = resp
		targetNode = candidate
		break
	}

	if nodeResp == nil {
		errEvt, _ := json.Marshal(map[string]string{"phase": "error", "error": "all nodes failed"})
		fmt.Fprintf(w, "%s\n", errEvt)
		if canFlush {
			flusher.Flush()
		}
		return
	}

	// Record in state
	h.state.SetVM(&state.VM{
		Name:   nodeResp.Name,
		NodeID: targetNode.ID,
		Status: nodeResp.Status,
		Labels: req.Labels,
	})

	log.Printf("VM created: %s on node %s", nodeResp.Name, targetNode.ID)

	ready, _ := json.Marshal(map[string]interface{}{
		"phase":        "ready",
		"name":         nodeResp.Name,
		"node":         targetNode.TailscaleName,
		"tailscale_ip": nodeResp.TailscaleIP,
		"mode":         nodeResp.Mode,
		"vcpu":         req.VCPU,
		"ram_mib":      req.RAMMIB,
		"disk":         req.Disk,
		"status":       nodeResp.Status,
	})
	fmt.Fprintf(w, "%s\n", ready)
	if canFlush {
		flusher.Flush()
	}
}

// vmListEntry is the enriched VM info returned by the list endpoint.
type vmListEntry struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Description string            `json:"description,omitempty"`
	NodeID      string            `json:"node_id"`
	NodeName    string            `json:"node_name"`
	TailscaleIP string            `json:"tailscale_ip"`
	Mode        string            `json:"mode"`
	VCPU        int               `json:"vcpu"`
	RAMMIB      int               `json:"ram_mib"`
	Disk        string            `json:"disk"`
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels,omitempty"`
}

func (h *Handler) handleVMList(w http.ResponseWriter, r *http.Request) {
	vms := h.state.ListVMs()

	// Group VMs by node so we make one request per node
	nodeVMs := make(map[string][]*state.VM)
	for _, v := range vms {
		nodeVMs[v.NodeID] = append(nodeVMs[v.NodeID], v)
	}

	// Fetch details from each node in parallel
	type nodeResult struct {
		nodeID string
		vms    map[string]*node.VMDetail
	}
	results := make(chan nodeResult, len(nodeVMs))

	for nodeID := range nodeVMs {
		go func(nid string) {
			nr := nodeResult{nodeID: nid, vms: make(map[string]*node.VMDetail)}
			n := h.state.GetNode(nid)
			if n == nil || n.APIAddr == "" {
				results <- nr
				return
			}
			fc := node.NewFastClient(n.APIAddr)
			vmList := fc.ListVMs()
			for i := range vmList {
				nr.vms[vmList[i].Name] = &vmList[i]
			}
			results <- nr
		}(nodeID)
	}

	detailsByNode := make(map[string]map[string]*node.VMDetail)
	for range nodeVMs {
		nr := <-results
		detailsByNode[nr.nodeID] = nr.vms
	}

	var entries []vmListEntry
	for _, v := range vms {
		n := h.state.GetNode(v.NodeID)
		nodeName := v.NodeID
		if n != nil {
			nodeName = n.TailscaleName
		}

		entry := vmListEntry{
			Name:     v.Name,
			NodeID:   v.NodeID,
			NodeName: nodeName,
			Status:   v.Status,
			Labels:   v.Labels,
		}

		if nodeDetail, ok := detailsByNode[v.NodeID]; ok {
			if detail, ok := nodeDetail[v.Name]; ok {
				entry.Type = detail.Type
				entry.Description = detail.Description
				entry.TailscaleIP = detail.TailscaleIP
				entry.Mode = detail.Mode
				entry.VCPU = detail.VCPU
				entry.RAMMIB = detail.RAMMIB
				entry.Disk = detail.Disk
				entry.Status = detail.Status
			}
		}

		entries = append(entries, entry)
	}

	writeJSON(w, entries)
}

func (h *Handler) handleVMGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/vms/")
	}
	vm := h.state.GetVM(name)
	if vm == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	n := h.state.GetNode(vm.NodeID)
	result := map[string]interface{}{
		"name":    vm.Name,
		"node_id": vm.NodeID,
		"status":  vm.Status,
	}
	if n != nil {
		result["node_name"] = n.TailscaleName

		fc := node.NewFastClient(n.APIAddr)
		if detail := fc.GetVM(name); detail != nil {
			result["tailscale_ip"] = detail.TailscaleIP
			result["mode"] = detail.Mode
			result["vcpu"] = detail.VCPU
			result["ram_mib"] = detail.RAMMIB
			result["disk"] = detail.Disk
			result["status"] = detail.Status
		}
	}

	writeJSON(w, result)
}

func (h *Handler) handleVMLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/vms/")
		name = strings.TrimSuffix(name, "/logs")
	}

	vm := h.state.GetVM(name)
	if vm == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	n := h.state.GetNode(vm.NodeID)
	if n == nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}

	lines := r.URL.Query().Get("lines")
	url := fmt.Sprintf("http://%s/api/vms/%s/logs", n.APIAddr, name)
	if lines != "" {
		url += "?lines=" + lines
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		http.Error(w, "node unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (h *Handler) handleVMUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/vms/")
	}

	vm := h.state.GetVM(name)
	if vm == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	n := h.state.GetNode(vm.NodeID)
	if n == nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}

	body, _ := io.ReadAll(r.Body)
	url := fmt.Sprintf("http://%s/api/vms/%s", n.APIAddr, name)
	req, _ := http.NewRequest("PATCH", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "node unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (h *Handler) handleVMDestroy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/vms/")
	}

	vm := h.state.GetVM(name)
	if vm == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	h.state.UpdateVMStatus(name, "destroying")

	n := h.state.GetNode(vm.NodeID)
	if n != nil {
		client := node.NewClient(n.APIAddr)
		if err := client.Destroy(name); err != nil {
			log.Printf("Node destroy failed for %s: %v (cleaning up state anyway)", name, err)
		}
	}

	h.state.DeleteVM(name)
	log.Printf("VM destroyed: %s", name)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleVMStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractPathSegment(r.URL.Path, "/api/vms/", "/stop")
	}

	vm := h.state.GetVM(name)
	if vm == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	n := h.state.GetNode(vm.NodeID)
	if n == nil {
		http.Error(w, "node not found", http.StatusInternalServerError)
		return
	}

	client := node.NewClient(n.APIAddr)
	if err := client.Stop(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.state.UpdateVMStatus(name, "stopped")
	writeJSON(w, map[string]string{"status": "stopped"})
}

func (h *Handler) handleVMStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractPathSegment(r.URL.Path, "/api/vms/", "/start")
	}

	vm := h.state.GetVM(name)
	if vm == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	n := h.state.GetNode(vm.NodeID)
	if n == nil {
		http.Error(w, "node not found", http.StatusInternalServerError)
		return
	}

	client := node.NewClient(n.APIAddr)
	resp, err := client.Start(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.state.UpdateVMStatus(name, "running")
	writeJSON(w, resp)
}

func (h *Handler) handleVMCopy(w http.ResponseWriter, r *http.Request) {
	srcName := r.PathValue("name")
	if srcName == "" {
		srcName = extractPathSegment(r.URL.Path, "/api/vms/", "/copy")
	}

	var req struct {
		DstName string `json:"dst_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.DstName == "" {
		out, err := exec.Command("boxcutter-names", "generate").Output()
		if err != nil {
			http.Error(w, "failed to generate name", http.StatusInternalServerError)
			return
		}
		req.DstName = strings.TrimSpace(string(out))
	}

	if v := h.state.GetVM(req.DstName); v != nil {
		http.Error(w, fmt.Sprintf("VM '%s' already exists", req.DstName), http.StatusConflict)
		return
	}

	srcVM := h.state.GetVM(srcName)
	if srcVM == nil {
		http.Error(w, "source VM not found", http.StatusNotFound)
		return
	}

	n := h.state.GetNode(srcVM.NodeID)
	if n == nil {
		http.Error(w, "source node not found", http.StatusInternalServerError)
		return
	}

	flusher, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusCreated)

	client := node.NewClient(n.APIAddr)
	nodeResp, err := client.CopyStreaming(srcName, req.DstName, func(evt *node.ProgressEvent) {
		line, _ := json.Marshal(evt)
		fmt.Fprintf(w, "%s\n", line)
		if canFlush {
			flusher.Flush()
		}
	})
	if err != nil {
		log.Printf("Copy %s -> %s failed: %v", srcName, req.DstName, err)
		errEvt, _ := json.Marshal(map[string]string{"phase": "error", "error": err.Error()})
		fmt.Fprintf(w, "%s\n", errEvt)
		if canFlush {
			flusher.Flush()
		}
		return
	}

	h.state.SetVM(&state.VM{
		Name:   nodeResp.Name,
		NodeID: srcVM.NodeID,
		Status: nodeResp.Status,
	})

	log.Printf("VM copied: %s -> %s on node %s", srcName, nodeResp.Name, srcVM.NodeID)

	ready, _ := json.Marshal(map[string]interface{}{
		"phase":        "ready",
		"name":         nodeResp.Name,
		"node":         n.TailscaleName,
		"tailscale_ip": nodeResp.TailscaleIP,
		"mode":         nodeResp.Mode,
		"status":       nodeResp.Status,
	})
	fmt.Fprintf(w, "%s\n", ready)
	if canFlush {
		flusher.Flush()
	}
}

// --- VM repos ---

func (h *Handler) handleVMAddRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractPathSegment(r.URL.Path, "/api/vms/", "/repos")
	}
	vm := h.state.GetVM(name)
	if vm == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	var req struct {
		Repo string `json:"repo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Repo == "" {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return
	}
	n := h.state.GetNode(vm.NodeID)
	if n == nil {
		http.Error(w, "node not found", http.StatusInternalServerError)
		return
	}
	client := node.NewClient(n.APIAddr)
	repos, err := client.AddRepo(name, req.Repo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{"repos": repos})
}

func (h *Handler) handleVMRemoveRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	repo := r.PathValue("repo")
	vm := h.state.GetVM(name)
	if vm == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	n := h.state.GetNode(vm.NodeID)
	if n == nil {
		http.Error(w, "node not found", http.StatusInternalServerError)
		return
	}
	client := node.NewClient(n.APIAddr)
	repos, err := client.RemoveRepo(name, repo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{"repos": repos})
}

func (h *Handler) handleVMListRepos(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/vms/")
	}
	vm := h.state.GetVM(name)
	if vm == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	n := h.state.GetNode(vm.NodeID)
	if n == nil {
		http.Error(w, "node not found", http.StatusInternalServerError)
		return
	}
	client := node.NewClient(n.APIAddr)
	repos, err := client.ListRepos(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{"repos": repos})
}

// --- VM projects ---

func (h *Handler) handleVMAddProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct{ Project string `json:"project"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Project == "" {
		http.Error(w, "project is required", http.StatusBadRequest)
		return
	}
	v := h.state.GetVM(name)
	if v == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	n := h.state.GetNode(v.NodeID)
	if n == nil || n.APIAddr == "" {
		http.Error(w, "node not available", http.StatusServiceUnavailable)
		return
	}
	nc := node.NewClient(n.APIAddr)
	projects, err := nc.AddProject(name, req.Project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{"projects": projects})
}

func (h *Handler) handleVMRemoveProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	project := r.PathValue("project")
	v := h.state.GetVM(name)
	if v == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	n := h.state.GetNode(v.NodeID)
	if n == nil || n.APIAddr == "" {
		http.Error(w, "node not available", http.StatusServiceUnavailable)
		return
	}
	nc := node.NewClient(n.APIAddr)
	projects, err := nc.RemoveProject(name, project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{"projects": projects})
}

func (h *Handler) handleVMListProjects(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	v := h.state.GetVM(name)
	if v == nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	n := h.state.GetNode(v.NodeID)
	if n == nil || n.APIAddr == "" {
		http.Error(w, "node not available", http.StatusServiceUnavailable)
		return
	}
	nc := node.NewClient(n.APIAddr)
	projects, err := nc.ListProjects(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{"projects": projects})
}

// --- Golden images ---

type goldenListEntry struct {
	Version string   `json:"version"`
	Nodes   []string `json:"nodes"`
}

func (h *Handler) handleGoldenList(w http.ResponseWriter, r *http.Request) {
	images := h.state.ListGoldenImages()

	versionNodes := make(map[string][]string)
	var order []string
	for _, img := range images {
		if _, seen := versionNodes[img.Version]; !seen {
			order = append(order, img.Version)
		}
		nodeName := img.NodeID
		if n := h.state.GetNode(img.NodeID); n != nil {
			nodeName = n.TailscaleName
		}
		versionNodes[img.Version] = append(versionNodes[img.Version], nodeName)
	}

	var entries []goldenListEntry
	for _, ver := range order {
		entries = append(entries, goldenListEntry{
			Version: ver,
			Nodes:   versionNodes[ver],
		})
	}
	writeJSON(w, entries)
}

// --- Golden head ---

func (h *Handler) handleGoldenGetHead(w http.ResponseWriter, r *http.Request) {
	head := h.db.GetGoldenHead()
	writeJSON(w, map[string]string{"version": head})
}

func (h *Handler) handleGoldenSetHead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Version == "" {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}

	if err := h.db.SetGoldenHead(req.Version); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if h.mqtt != nil {
		if err := h.mqtt.PublishGoldenHead(req.Version); err != nil {
			log.Printf("mqtt: failed to publish golden head: %v", err)
		}
	}

	log.Printf("Golden head set to %s", req.Version)
	writeJSON(w, map[string]string{"version": req.Version, "status": "ok"})
}

// --- Health ---

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	nodes := h.state.ListNodes()
	vms := h.state.ListVMs()

	var activeNodes int
	var totalRAM, allocRAM int

	var wg sync.WaitGroup
	type healthResult struct {
		ramTotal int
		ramAlloc int
	}
	healthResults := make([]healthResult, len(nodes))

	for i, n := range nodes {
		if n.Status != "active" {
			continue
		}
		activeNodes++
		wg.Add(1)
		go func(idx int, apiAddr string) {
			defer wg.Done()
			fc := node.NewFastClient(apiAddr)
			if health := fc.Health(); health != nil {
				healthResults[idx] = healthResult{
					ramTotal: health.RAMTotalMIB,
					ramAlloc: health.RAMAllocatedMIB,
				}
			}
		}(i, n.APIAddr)
	}
	wg.Wait()

	for _, hr := range healthResults {
		totalRAM += hr.ramTotal
		allocRAM += hr.ramAlloc
	}

	writeJSON(w, map[string]interface{}{
		"nodes_total":       len(nodes),
		"nodes_active":      activeNodes,
		"vms_total":         len(vms),
		"ram_total_mib":     totalRAM,
		"ram_allocated_mib": allocRAM,
	})
}

// --- SSH key handlers ---

type addKeysRequest struct {
	GitHubUser string   `json:"github_user"`
	Keys       []string `json:"keys"`
}

func (h *Handler) handleAddKeys(w http.ResponseWriter, r *http.Request) {
	var req addKeysRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.GitHubUser == "" || len(req.Keys) == 0 {
		http.Error(w, "github_user and keys are required", http.StatusBadRequest)
		return
	}

	added, err := h.db.AddSSHKeys(req.GitHubUser, req.Keys, time.Now().Format(time.RFC3339))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Added %d SSH key(s) for %s", added, req.GitHubUser)
	writeJSON(w, map[string]interface{}{
		"github_user": req.GitHubUser,
		"keys_added":  added,
	})
}

func (h *Handler) handleListKeys(w http.ResponseWriter, r *http.Request) {
	entries, err := h.db.ListSSHKeyEntries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, entries)
}

func (h *Handler) handleDeleteKeys(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	if user == "" {
		user = extractName(r.URL.Path, "/api/keys/")
	}
	if err := h.db.DeleteSSHKeysByUser(user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func extractName(path, prefix string) string {
	s := strings.TrimPrefix(path, prefix)
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i]
	}
	return s
}

func extractPathSegment(path, prefix, suffix string) string {
	s := strings.TrimPrefix(path, prefix)
	s = strings.TrimSuffix(s, suffix)
	return s
}

// --- Tapegun handlers ---

type tapegunActivityEntry struct {
	Name        string                      `json:"name"`
	NodeID      string                      `json:"node_id"`
	NodeName    string                      `json:"node_name"`
	VMStatus    string                      `json:"vm_status"`
	Activity    *node.TapegunActivityReport `json:"activity,omitempty"`
	AgentStatus *node.TapegunStatusReport   `json:"agent_status,omitempty"`
	PendingMsgs int                         `json:"pending_messages"`
}

func (h *Handler) handleTapegunActivity(w http.ResponseWriter, r *http.Request) {
	vms := h.state.ListVMs()

	nodeVMs := make(map[string][]*state.VM)
	for _, v := range vms {
		nodeVMs[v.NodeID] = append(nodeVMs[v.NodeID], v)
	}

	type nodeResult struct {
		nodeID   string
		activity map[string]*node.TapegunVMActivity
	}
	results := make(chan nodeResult, len(nodeVMs))

	for nodeID := range nodeVMs {
		go func(nid string) {
			nr := nodeResult{nodeID: nid, activity: make(map[string]*node.TapegunVMActivity)}
			n := h.state.GetNode(nid)
			if n == nil || n.APIAddr == "" {
				results <- nr
				return
			}
			fc := node.NewFastClient(n.APIAddr)
			activities := fc.GetAllActivity()
			for i := range activities {
				nr.activity[activities[i].VMID] = &activities[i]
			}
			results <- nr
		}(nodeID)
	}

	activityByNode := make(map[string]map[string]*node.TapegunVMActivity)
	for range nodeVMs {
		nr := <-results
		activityByNode[nr.nodeID] = nr.activity
	}

	var entries []tapegunActivityEntry
	for _, v := range vms {
		n := h.state.GetNode(v.NodeID)
		nodeName := v.NodeID
		if n != nil {
			nodeName = n.TailscaleName
		}

		entry := tapegunActivityEntry{
			Name:     v.Name,
			NodeID:   v.NodeID,
			NodeName: nodeName,
			VMStatus: v.Status,
		}

		if nodeAct, ok := activityByNode[v.NodeID]; ok {
			if act, ok := nodeAct[v.Name]; ok {
				entry.Activity = act.LastActivity
				entry.AgentStatus = act.LastStatus
				entry.PendingMsgs = act.PendingMessages
			}
		}

		entries = append(entries, entry)
	}

	writeJSON(w, entries)
}

func (h *Handler) handleTapegunVMActivity(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/tapegun/activity/")
	}

	v := h.state.GetVM(name)
	if v == nil {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}

	n := h.state.GetNode(v.NodeID)
	if n == nil || n.APIAddr == "" {
		http.Error(w, "node not available", http.StatusServiceUnavailable)
		return
	}

	fc := node.NewFastClient(n.APIAddr)
	activities := fc.GetAllActivity()
	for _, act := range activities {
		if act.VMID == name {
			nodeName := v.NodeID
			if n != nil {
				nodeName = n.TailscaleName
			}
			writeJSON(w, tapegunActivityEntry{
				Name:        v.Name,
				NodeID:      v.NodeID,
				NodeName:    nodeName,
				VMStatus:    v.Status,
				Activity:    act.LastActivity,
				AgentStatus: act.LastStatus,
				PendingMsgs: act.PendingMessages,
			})
			return
		}
	}

	nodeName := v.NodeID
	if n != nil {
		nodeName = n.TailscaleName
	}
	writeJSON(w, tapegunActivityEntry{
		Name:     v.Name,
		NodeID:   v.NodeID,
		NodeName: nodeName,
		VMStatus: v.Status,
	})
}

func (h *Handler) handleTapegunMessage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/tapegun/message/")
	}

	var msg node.TapegunMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if msg.ID == "" {
		msg.ID = fmt.Sprintf("tg-%d", time.Now().UnixNano())
	}
	if msg.CreatedAt == "" {
		msg.CreatedAt = time.Now().Format(time.RFC3339)
	}

	v := h.state.GetVM(name)
	if v == nil {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}

	n := h.state.GetNode(v.NodeID)
	if n == nil || n.APIAddr == "" {
		h.queueMessage(name, msg)
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"status": "queued", "message_id": msg.ID, "reason": "node unavailable"})
		return
	}

	nc := node.NewClient(n.APIAddr)
	if err := nc.SendTapegunMessage(name, &msg); err != nil {
		h.queueMessage(name, msg)
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"status": "queued", "message_id": msg.ID, "reason": "delivery failed, will retry"})
		return
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"status": "sent", "message_id": msg.ID})
}

func (h *Handler) handleTapegunBroadcast(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body     string `json:"body"`
		From     string `json:"from"`
		Priority string `json:"priority"`
		SendKeys bool   `json:"send_keys,omitempty"`
		NoEnter  bool   `json:"no_enter,omitempty"`
		Filter   string `json:"filter,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Body == "" {
		http.Error(w, "body is required", http.StatusBadRequest)
		return
	}
	if req.Priority == "" {
		req.Priority = "normal"
	}

	vms := h.state.ListVMs()

	var sent []string
	var failed []string

	for _, v := range vms {
		if req.Filter != "" && v.Status != req.Filter {
			continue
		}

		n := h.state.GetNode(v.NodeID)
		if n == nil || n.APIAddr == "" {
			failed = append(failed, v.Name)
			continue
		}

		msg := &node.TapegunMessage{
			ID:        fmt.Sprintf("tg-%d-%s", time.Now().UnixNano(), v.Name),
			From:      req.From,
			Body:      req.Body,
			Priority:  req.Priority,
			SendKeys:  req.SendKeys,
			NoEnter:   req.NoEnter,
			CreatedAt: time.Now().Format(time.RFC3339),
		}

		nc := node.NewClient(n.APIAddr)
		if err := nc.SendTapegunMessage(v.Name, msg); err != nil {
			failed = append(failed, v.Name)
			continue
		}
		sent = append(sent, v.Name)
	}

	writeJSON(w, map[string]interface{}{
		"sent":   sent,
		"failed": failed,
	})
}

func (h *Handler) handleVMExec(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Command == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}

	v := h.state.GetVM(name)
	if v == nil {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}

	n := h.state.GetNode(v.NodeID)
	if n == nil || n.APIAddr == "" {
		http.Error(w, "node not available", http.StatusServiceUnavailable)
		return
	}

	nc := node.NewClient(n.APIAddr)
	result, err := nc.ExecInVM(name, req.Command)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, result)
}

func (h *Handler) handleVMCopyTo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	dstPath := r.URL.Query().Get("path")
	if dstPath == "" {
		http.Error(w, "path query parameter is required", http.StatusBadRequest)
		return
	}

	v := h.state.GetVM(name)
	if v == nil {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	n := h.state.GetNode(v.NodeID)
	if n == nil || n.APIAddr == "" {
		http.Error(w, "node not available", http.StatusServiceUnavailable)
		return
	}

	nc := node.NewClient(n.APIAddr)
	if err := nc.CopyToVM(name, dstPath, r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleVMCopyFrom(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	srcPath := r.URL.Query().Get("path")
	if srcPath == "" {
		http.Error(w, "path query parameter is required", http.StatusBadRequest)
		return
	}

	v := h.state.GetVM(name)
	if v == nil {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	n := h.state.GetNode(v.NodeID)
	if n == nil || n.APIAddr == "" {
		http.Error(w, "node not available", http.StatusServiceUnavailable)
		return
	}

	nc := node.NewClient(n.APIAddr)
	w.Header().Set("Content-Type", "application/octet-stream")
	if err := nc.CopyFromVM(name, srcPath, w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (h *Handler) handleVMLocation(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "vm name required", http.StatusBadRequest)
		return
	}

	v := h.state.GetVM(name)
	if v == nil {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}

	n := h.state.GetNode(v.NodeID)
	if n == nil || n.APIAddr == "" {
		http.Error(w, "node not available", http.StatusServiceUnavailable)
		return
	}

	writeJSON(w, map[string]string{
		"vm":        name,
		"node_id":   n.ID,
		"node_addr": n.APIAddr,
	})
}

// --- Tapegun message queue ---

const messageQueueTTL = 30 * time.Minute

func (h *Handler) queueMessage(vmName string, msg node.TapegunMessage) {
	h.msgQueueMu.Lock()
	defer h.msgQueueMu.Unlock()
	h.msgQueue = append(h.msgQueue, queuedMessage{
		vmName:   vmName,
		msg:      msg,
		queuedAt: time.Now(),
	})
	log.Printf("tapegun: queued message %s for VM %s (queue size: %d)", msg.ID, vmName, len(h.msgQueue))
}

func (h *Handler) messageDeliveryLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		h.msgQueueMu.Lock()
		if len(h.msgQueue) == 0 {
			h.msgQueueMu.Unlock()
			continue
		}

		pending := make([]queuedMessage, len(h.msgQueue))
		copy(pending, h.msgQueue)
		h.msgQueue = h.msgQueue[:0]
		h.msgQueueMu.Unlock()

		var requeue []queuedMessage
		for _, qm := range pending {
			if time.Since(qm.queuedAt) > messageQueueTTL {
				log.Printf("tapegun: expired queued message %s for VM %s (age: %s)",
					qm.msg.ID, qm.vmName, time.Since(qm.queuedAt).Round(time.Second))
				continue
			}

			v := h.state.GetVM(qm.vmName)
			if v == nil {
				requeue = append(requeue, qm)
				continue
			}
			n := h.state.GetNode(v.NodeID)
			if n == nil || n.APIAddr == "" {
				requeue = append(requeue, qm)
				continue
			}
			nc := node.NewClient(n.APIAddr)
			if err := nc.SendTapegunMessage(qm.vmName, &qm.msg); err != nil {
				requeue = append(requeue, qm)
				continue
			}
			log.Printf("tapegun: delivered queued message %s to VM %s (was queued %s)",
				qm.msg.ID, qm.vmName, time.Since(qm.queuedAt).Round(time.Second))
		}

		if len(requeue) > 0 {
			h.msgQueueMu.Lock()
			h.msgQueue = append(h.msgQueue, requeue...)
			h.msgQueueMu.Unlock()
		}
	}
}

// --- Work queue ---

func (h *Handler) initWorkQueue() {
	repo := os.Getenv("QUEUE_REPO")
	label := os.Getenv("QUEUE_LABEL")
	if label == "" {
		label = "ready"
	}

	cfg := queue.QueueConfig{
		Repo:           repo,
		Label:          label,
		PollInterval:   60 * time.Second,
		AssignInterval: 15 * time.Second,
		TimeoutMinutes: 30,
		PriorityLabels: map[string]int{
			"p0-critical": 0,
			"p1-high":     1,
			"bug":         1,
			"p2-medium":   2,
			"feature":     3,
			"p3-low":      3,
		},
	}

	q := queue.New(h.db.Conn(), cfg)
	if err := q.Migrate(); err != nil {
		log.Printf("queue: migration failed: %v", err)
		return
	}

	h.workQueue = q
	h.dispatcher = queue.NewDispatcher(q, h.getIdleWorkers, h.assignWorkToVM)

	if repo != "" {
		h.dispatcher.Start()
		log.Printf("queue: started dispatcher (repo=%s, label=%s)", repo, label)
	} else {
		log.Printf("queue: no QUEUE_REPO set, dispatcher not started (manual mode only)")
	}
}

// getIdleWorkers returns VMs whose tapegun status is "idle" and have no assigned work.
func (h *Handler) getIdleWorkers() []queue.IdleWorker {
	vms := h.state.ListVMs()

	nodeVMs := make(map[string][]*state.VM)
	for _, v := range vms {
		if v.Status != "running" {
			continue
		}
		nodeVMs[v.NodeID] = append(nodeVMs[v.NodeID], v)
	}

	type nodeResult struct {
		nodeID   string
		activity map[string]*node.TapegunVMActivity
	}
	results := make(chan nodeResult, len(nodeVMs))

	for nodeID := range nodeVMs {
		go func(nid string) {
			nr := nodeResult{nodeID: nid, activity: make(map[string]*node.TapegunVMActivity)}
			n := h.state.GetNode(nid)
			if n == nil || n.APIAddr == "" {
				results <- nr
				return
			}
			fc := node.NewFastClient(n.APIAddr)
			activities := fc.GetAllActivity()
			for i := range activities {
				nr.activity[activities[i].VMID] = &activities[i]
			}
			results <- nr
		}(nodeID)
	}

	activityByNode := make(map[string]map[string]*node.TapegunVMActivity)
	for range nodeVMs {
		nr := <-results
		activityByNode[nr.nodeID] = nr.activity
	}

	var idle []queue.IdleWorker
	for _, v := range vms {
		if v.Status != "running" {
			continue
		}
		nodeAct, ok := activityByNode[v.NodeID]
		if !ok {
			continue
		}
		act, ok := nodeAct[v.Name]
		if !ok {
			continue
		}
		if act.LastStatus != nil && act.LastStatus.Status == "idle" {
			team := v.Labels["team"]
			idle = append(idle, queue.IdleWorker{
				Name:   v.Name,
				NodeID: v.NodeID,
				Team:   team,
			})
		}
	}
	return idle
}

// assignWorkToVM delivers a work assignment to a VM via tapegun inbox message.
func (h *Handler) assignWorkToVM(vmName, messageBody string) error {
	v := h.state.GetVM(vmName)
	if v == nil {
		return fmt.Errorf("vm %s not found", vmName)
	}
	n := h.state.GetNode(v.NodeID)
	if n == nil || n.APIAddr == "" {
		return fmt.Errorf("node for %s not reachable", vmName)
	}

	msg := node.TapegunMessage{
		ID:       fmt.Sprintf("wq-%d", time.Now().UnixNano()),
		From:     "work-queue",
		Body:     messageBody,
		Priority: "normal",
		SendKeys: false,
	}

	nc := node.NewClient(n.APIAddr)
	return nc.SendTapegunMessage(vmName, &msg)
}

func (h *Handler) handleQueueList(w http.ResponseWriter, r *http.Request) {
	if h.workQueue == nil {
		http.Error(w, "work queue not initialized", http.StatusServiceUnavailable)
		return
	}
	statusFilter := r.URL.Query().Get("status")
	items := h.workQueue.List(statusFilter)
	writeJSON(w, items)
}

func (h *Handler) handleQueueAdd(w http.ResponseWriter, r *http.Request) {
	if h.workQueue == nil {
		http.Error(w, "work queue not initialized", http.StatusServiceUnavailable)
		return
	}

	var item queue.WorkItem
	if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if item.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	if item.Source == "" {
		item.Source = "manual"
	}

	if err := h.workQueue.Enqueue(&item); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, item)
}

func (h *Handler) handleQueueStats(w http.ResponseWriter, r *http.Request) {
	if h.workQueue == nil || h.dispatcher == nil {
		http.Error(w, "work queue not initialized", http.StatusServiceUnavailable)
		return
	}
	stats := h.dispatcher.Stats()
	writeJSON(w, stats)
}

func (h *Handler) handleQueueComplete(w http.ResponseWriter, r *http.Request) {
	if h.workQueue == nil {
		http.Error(w, "work queue not initialized", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if err := h.workQueue.Complete(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"status": "completed", "id": id})
}

func (h *Handler) handleQueueReassign(w http.ResponseWriter, r *http.Request) {
	if h.workQueue == nil {
		http.Error(w, "work queue not initialized", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if err := h.workQueue.Reassign(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "reassigned", "id": id})
}

func (h *Handler) handleQueueSync(w http.ResponseWriter, r *http.Request) {
	if h.workQueue == nil {
		http.Error(w, "work queue not initialized", http.StatusServiceUnavailable)
		return
	}
	n, err := h.workQueue.SyncGitHubIssues()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{"synced": n})
}

// --- Alert detection and SSE ---

// alertDetectionLoop runs every 15s, fetches activity from all nodes,
// detects state transitions, and emits alerts.
func (h *Handler) alertDetectionLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		h.detectAlerts()
	}
}

func (h *Handler) detectAlerts() {
	vms := h.state.ListVMs()
	if len(vms) == 0 {
		return
	}

	// Group VMs by node
	nodeVMs := make(map[string][]string)
	for _, v := range vms {
		nodeVMs[v.NodeID] = append(nodeVMs[v.NodeID], v.Name)
	}

	// Fetch activity from each node concurrently
	results := make(chan struct {
		nodeID string
		acts   []node.TapegunVMActivity
	}, len(nodeVMs))

	for nodeID := range nodeVMs {
		go func(nid string) {
			n := h.state.GetNode(nid)
			if n == nil || n.APIAddr == "" {
				results <- struct {
					nodeID string
					acts   []node.TapegunVMActivity
				}{nid, nil}
				return
			}
			fc := node.NewFastClient(n.APIAddr)
			acts := fc.GetAllActivity()
			results <- struct {
				nodeID string
				acts   []node.TapegunVMActivity
			}{nid, acts}
		}(nodeID)
	}

	// Build activity map: vmName → activity
	actByVM := make(map[string]*node.TapegunVMActivity)
	for range nodeVMs {
		r := <-results
		for i := range r.acts {
			actByVM[r.acts[i].VMID] = &r.acts[i]
		}
	}

	now := time.Now()
	h.prevStatusMu.Lock()
	defer h.prevStatusMu.Unlock()

	for _, v := range vms {
		act := actByVM[v.Name]

		// Determine current status
		curStatus := "unknown"
		curHealth := "unknown"
		if act != nil && act.LastActivity != nil {
			curStatus = act.LastActivity.Status
			h.lastReport[v.Name] = now
		}

		// Detect unresponsive: no report for >60s
		if lastSeen, ok := h.lastReport[v.Name]; ok && now.Sub(lastSeen) > 60*time.Second {
			if h.prevStatus[v.Name] != "unresponsive" {
				h.emitAlert(v.Name, "unresponsive", "No activity report for >60s")
			}
			curStatus = "unresponsive"
		}

		// Detect pane content errors
		if act != nil && act.LastActivity != nil {
			pane := act.LastActivity.PaneContent
			if containsError(pane) {
				if h.prevStatus[v.Name] != "error" {
					errType := detectErrorType(pane)
					h.emitAlert(v.Name, "error", errType)
				}
				curStatus = "error"
			}
		}

		prevSt := h.prevStatus[v.Name]
		prevHl := h.prevHealth[v.Name]

		// Status transitions
		if prevSt != "" && prevSt != curStatus {
			switch {
			case (prevSt == "active" || prevSt == "idle") && curStatus == "stopped":
				h.emitAlert(v.Name, "crash", "Agent session lost")
			case prevSt == "active" && curStatus == "idle":
				h.emitAlert(v.Name, "idle", "Agent stopped working")
			case (prevSt == "stopped" || prevSt == "idle" || prevSt == "error" || prevSt == "unresponsive") && curStatus == "active":
				h.emitAlert(v.Name, "recovery", "Agent resumed working")
			}
		}

		// Health transitions
		if prevHl != "" && prevHl != curHealth && curHealth != "unknown" {
			switch {
			case curHealth == "degraded":
				h.emitAlert(v.Name, "degraded", "Some health checks failing")
			case curHealth == "unhealthy":
				h.emitAlert(v.Name, "unhealthy", "VM in bad state")
			case (prevHl == "degraded" || prevHl == "unhealthy") && curHealth == "healthy":
				h.emitAlert(v.Name, "recovery", "Health restored")
			}
		}

		h.prevStatus[v.Name] = curStatus
		h.prevHealth[v.Name] = curHealth
	}
}

func containsError(pane string) bool {
	patterns := []string{
		"401 Unauthorized",
		"token expired",
		"FATAL",
		"Out of memory",
		"OOMKilled",
	}
	for _, p := range patterns {
		if strings.Contains(pane, p) {
			return true
		}
	}
	return false
}

func detectErrorType(pane string) string {
	if strings.Contains(pane, "401 Unauthorized") || strings.Contains(pane, "token expired") {
		return "Authentication error detected in pane output"
	}
	if strings.Contains(pane, "Out of memory") || strings.Contains(pane, "OOMKilled") {
		return "Out of memory error detected"
	}
	if strings.Contains(pane, "FATAL") {
		return "Fatal error detected in pane output"
	}
	return "Error detected in pane output"
}

// teamForVM extracts team name from VM name prefix heuristic.
func teamForVM(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) >= 3 {
		last := parts[len(parts)-1]
		isNum := len(last) > 0
		for _, c := range last {
			if c < '0' || c > '9' {
				isNum = false
				break
			}
		}
		if isNum {
			return strings.Join(parts[:len(parts)-2], "-")
		}
	}
	return ""
}

func (h *Handler) emitAlert(vmName, alertType, message string) {
	evt := AlertEvent{
		Timestamp: time.Now().Format(time.RFC3339),
		VMName:    vmName,
		Type:      alertType,
		Message:   message,
		Team:      teamForVM(vmName),
	}
	log.Printf("ALERT [%s] %s: %s", alertType, vmName, message)

	// Add to ring buffer
	h.alertsMu.Lock()
	h.alerts = append(h.alerts, evt)
	if len(h.alerts) > 200 {
		h.alerts = h.alerts[len(h.alerts)-200:]
	}
	h.alertsMu.Unlock()

	// Broadcast to SSE clients
	h.sseClientsMu.Lock()
	for ch := range h.sseClients {
		select {
		case ch <- evt:
		default: // drop if client is slow
		}
	}
	h.sseClientsMu.Unlock()
}

// handleAlerts returns recent alert events as JSON.
func (h *Handler) handleAlerts(w http.ResponseWriter, r *http.Request) {
	h.alertsMu.RLock()
	alerts := make([]AlertEvent, len(h.alerts))
	copy(alerts, h.alerts)
	h.alertsMu.RUnlock()
	writeJSON(w, alerts)
}

// handleAlertStream streams alerts via Server-Sent Events.
func (h *Handler) handleAlertStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan AlertEvent, 50)
	h.sseClientsMu.Lock()
	h.sseClients[ch] = true
	h.sseClientsMu.Unlock()

	defer func() {
		h.sseClientsMu.Lock()
		delete(h.sseClients, ch)
		h.sseClientsMu.Unlock()
	}()

	// Send initial heartbeat
	fmt.Fprintf(w, "event: heartbeat\ndata: {\"timestamp\":%q}\n\n", time.Now().Format(time.RFC3339))
	flusher.Flush()

	// Heartbeat ticker
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-ch:
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "event: alert\ndata: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, "event: heartbeat\ndata: {\"timestamp\":%q}\n\n", time.Now().Format(time.RFC3339))
			flusher.Flush()
		}
	}
}

// --- Metrics handlers ---

// handleMetrics returns token usage and cost metrics for all VMs.
func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	h.vmMetricsMu.RLock()
	all := make([]*VMMetrics, 0, len(h.vmMetrics))
	for _, m := range h.vmMetrics {
		copy := *m
		all = append(all, &copy)
	}
	h.vmMetricsMu.RUnlock()
	writeJSON(w, all)
}

// handleMetricsUpdate receives token usage metrics from a VM (via node agent proxy).
func (h *Handler) handleMetricsUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "VM name required", http.StatusBadRequest)
		return
	}

	var incoming struct {
		APICalls          int    `json:"api_calls"`
		InputTokens       int    `json:"input_tokens"`
		CacheReadTokens   int    `json:"cache_read_tokens"`
		CacheCreateTokens int    `json:"cache_create_tokens"`
		OutputTokens      int    `json:"output_tokens"`
		Model             string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	h.vmMetricsMu.Lock()
	m := h.vmMetrics[name]
	if m == nil {
		m = &VMMetrics{VMName: name, Team: teamForVM(name)}
		h.vmMetrics[name] = m
	}
	m.APICalls = incoming.APICalls
	m.InputTokens = incoming.InputTokens
	m.CacheReadTokens = incoming.CacheReadTokens
	m.CacheCreateTokens = incoming.CacheCreateTokens
	m.OutputTokens = incoming.OutputTokens
	m.Model = incoming.Model
	m.EstimatedCostUSD = estimateCost(m)
	m.LastUpdated = time.Now().Format(time.RFC3339)
	h.vmMetricsMu.Unlock()

	w.WriteHeader(http.StatusOK)
	writeJSON(w, m)
}

// --- Team collaboration ---

func (h *Handler) handleTeamMessage(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")

	var msg collab.TeamMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	msg.Team = team
	if msg.Type == "" {
		msg.Type = "notification"
	}

	h.collabHub.SendMessage(&msg)

	// If targeted, also deliver immediately via tapegun inbox
	if msg.To != "" {
		h.deliverTeamMessage(&msg)
	} else {
		// Broadcast: deliver to all team members
		vms := h.state.ListVMsByLabel("team", team)
		for _, v := range vms {
			msgCopy := msg
			msgCopy.To = v.Name
			h.deliverTeamMessage(&msgCopy)
		}
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"status": "sent", "id": msg.ID})
}

func (h *Handler) deliverTeamMessage(msg *collab.TeamMessage) {
	v := h.state.GetVM(msg.To)
	if v == nil {
		return
	}
	n := h.state.GetNode(v.NodeID)
	if n == nil || n.APIAddr == "" {
		return
	}

	body := fmt.Sprintf("[%s] from %s: %s", strings.ToUpper(msg.Type), msg.From, msg.Body)
	if msg.Subject != "" {
		body = fmt.Sprintf("[%s] from %s — %s\n%s", strings.ToUpper(msg.Type), msg.From, msg.Subject, msg.Body)
	}

	tapegunMsg := node.TapegunMessage{
		ID:       msg.ID,
		From:     "team:" + msg.From,
		Body:     body,
		Priority: "normal",
	}

	nc := node.NewClient(n.APIAddr)
	if err := nc.SendTapegunMessage(msg.To, &tapegunMsg); err != nil {
		h.queueMessage(msg.To, tapegunMsg)
	}
}

func (h *Handler) handleTeamMessages(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	agentVM := r.URL.Query().Get("agent")

	if agentVM != "" {
		msgs := h.collabHub.PendingMessages(team, agentVM)
		writeJSON(w, msgs)
	} else {
		msgs := h.collabHub.ListMessages(team)
		writeJSON(w, msgs)
	}
}

func (h *Handler) handleTeamReportFiles(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")

	var report collab.FileReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	report.Team = team

	h.collabHub.ReportFiles(&report)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleTeamGetFiles(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	files := h.collabHub.GetFiles(team)
	writeJSON(w, files)
}

func (h *Handler) handleTeamConflicts(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	conflicts := h.collabHub.DetectConflicts(team)
	writeJSON(w, conflicts)
}

func (h *Handler) handleTeamScratchpadAppend(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")

	var entry collab.ScratchpadEntry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	h.collabHub.AppendScratchpad(team, &entry)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, entry)
}

func (h *Handler) handleTeamScratchpadGet(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	entries := h.collabHub.GetScratchpad(team)
	writeJSON(w, entries)
}

// conflictDetectionLoop periodically checks for file overlaps and alerts agents.
func (h *Handler) conflictDetectionLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Track which conflicts we've already alerted about
	alerted := make(map[string]time.Time)

	for range ticker.C {
		// Find all teams
		teams := make(map[string]bool)
		for _, v := range h.state.ListVMs() {
			if t := v.Labels["team"]; t != "" {
				teams[t] = true
			}
		}

		for team := range teams {
			conflicts := h.collabHub.DetectConflicts(team)
			for _, c := range conflicts {
				key := c.File + "|" + c.AgentA + "|" + c.AgentB
				if lastAlert, ok := alerted[key]; ok && time.Since(lastAlert) < 5*time.Minute {
					continue // don't re-alert within 5 minutes
				}
				alerted[key] = time.Now()

				// Alert both agents
				for _, agent := range []string{c.AgentA, c.AgentB} {
					alertMsg := collab.FormatConflictAlert(c, agent)
					h.collabHub.SendMessage(&collab.TeamMessage{
						From:    "conflict-detector",
						To:      agent,
						Team:    team,
						Type:    "conflict_alert",
						Subject: "File conflict: " + c.File,
						Body:    alertMsg,
					})
				}
				log.Printf("collab: conflict detected on %s between %s and %s", c.File, c.AgentA, c.AgentB)
			}
		}

		// Clean old alert entries
		for key, ts := range alerted {
			if time.Since(ts) > 10*time.Minute {
				delete(alerted, key)
			}
		}
	}
}
