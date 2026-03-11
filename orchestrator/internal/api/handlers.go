package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
	orchmqtt "github.com/AndrewBudd/boxcutter/orchestrator/internal/mqtt"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/node"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/scheduler"
)

type Handler struct {
	db   *db.DB
	mqtt *orchmqtt.Client

	// Migration state: when preparing for migration, mutating requests are rejected.
	// Auto-expires after migrateDeadline to prevent permanent lockout if migration fails.
	migrating       bool
	migrateDeadline time.Time
	migrateMu       sync.Mutex
}

func NewHandler(database *db.DB) *Handler {
	h := &Handler{db: database}
	go h.healthMonitorLoop()
	return h
}

// isMigrating returns true if this orchestrator is in pre-migrate mode.
// Automatically expires the migration state after the deadline.
func (h *Handler) isMigrating() bool {
	h.migrateMu.Lock()
	defer h.migrateMu.Unlock()
	if h.migrating && time.Now().After(h.migrateDeadline) {
		log.Printf("Migration mode expired (deadline passed), reverting to active")
		h.migrating = false
	}
	return h.migrating
}

// migrationGuard wraps a handler to reject requests when migrating.
func (h *Handler) migrationGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.isMigrating() {
			http.Error(w, "orchestrator is preparing for migration, please retry shortly", http.StatusServiceUnavailable)
			return
		}
		next(w, r)
	}
}

// SetMQTT sets the MQTT client for publishing golden head updates.
func (h *Handler) SetMQTT(mc *orchmqtt.Client) {
	h.mqtt = mc

	// On connect, publish the current golden head (if any) — non-blocking
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
		nodes, err := h.db.ListNodes()
		if err != nil {
			continue
		}
		for _, n := range nodes {
			if n.Status == "retired" || n.Status == "draining" || n.Status == "provisioning" {
				continue
			}
			fc := node.NewFastClient(n.APIAddr)
			health := fc.Health()
			if health == nil {
				if n.Status == "active" {
					log.Printf("Node %s: unreachable, marking down", n.ID)
					h.db.SetNodeStatus(n.ID, "down")
				}
				continue
			}
			if n.Status == "down" {
				log.Printf("Node %s: back up, marking active", n.ID)
				h.db.SetNodeStatus(n.ID, "active")
			}

			// Sync VM inventory from node
			if vmList := fc.ListVMs(); vmList != nil {
				var dbVMs []db.VM
				for _, v := range vmList {
					dbVMs = append(dbVMs, db.VM{
						Name:   v.Name,
						NodeID: n.ID,
						Status: v.Status,
					})
				}
				h.db.SyncNodeVMs(n.ID, dbVMs)
			}

			// Sync golden image versions from node
			if versions := fc.GoldenVersions(); versions != nil {
				now := time.Now().Format(time.RFC3339)
				h.db.DeleteGoldenImagesForNode(n.ID)
				for _, ver := range versions {
					h.db.UpsertGoldenImage(ver, n.ID, now)
				}
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

	// VM management (mutating endpoints guarded during migration)
	mux.HandleFunc("POST /api/vms", h.migrationGuard(h.handleVMCreate))
	mux.HandleFunc("GET /api/vms", h.handleVMList)
	mux.HandleFunc("GET /api/vms/{name}", h.handleVMGet)
	mux.HandleFunc("DELETE /api/vms/{name}", h.migrationGuard(h.handleVMDestroy))
	mux.HandleFunc("POST /api/vms/{name}/stop", h.migrationGuard(h.handleVMStop))
	mux.HandleFunc("POST /api/vms/{name}/start", h.migrationGuard(h.handleVMStart))
	mux.HandleFunc("POST /api/vms/{name}/copy", h.migrationGuard(h.handleVMCopy))
	mux.HandleFunc("POST /api/vms/{name}/repos", h.migrationGuard(h.handleVMAddRepo))
	mux.HandleFunc("DELETE /api/vms/{name}/repos/{repo...}", h.migrationGuard(h.handleVMRemoveRepo))
	mux.HandleFunc("GET /api/vms/{name}/repos", h.handleVMListRepos)

	// Golden images
	mux.HandleFunc("GET /api/golden", h.handleGoldenList)
	mux.HandleFunc("GET /api/golden/head", h.handleGoldenGetHead)
	mux.HandleFunc("POST /api/golden/head", h.handleGoldenSetHead)

	// Migration (self-migration for orchestrator upgrades)
	mux.HandleFunc("POST /api/migrate", h.handleMigrate)
	mux.HandleFunc("POST /api/prepare-migrate", h.handlePrepareMigrate)
	mux.HandleFunc("POST /api/shutdown", h.handleShutdown)

	// SSH keys
	mux.HandleFunc("POST /api/keys/add", h.handleAddKeys)
	mux.HandleFunc("GET /api/keys", h.handleListKeys)
	mux.HandleFunc("DELETE /api/keys/{user}", h.handleDeleteKeys)

	// Health
	mux.HandleFunc("GET /api/health", h.handleHealth)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	// Wingman
	mux.HandleFunc("GET /api/wingman/activity", h.handleWingmanActivity)
	mux.HandleFunc("GET /api/wingman/activity/{name}", h.handleWingmanVMActivity)
	mux.HandleFunc("POST /api/wingman/message/{name}", h.handleWingmanMessage)
	mux.HandleFunc("POST /api/wingman/broadcast", h.handleWingmanBroadcast)
}

// --- Node handlers ---

func (h *Handler) handleNodeRegister(w http.ResponseWriter, r *http.Request) {
	var n db.Node
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
	if n.RegisteredAt == "" {
		n.RegisteredAt = time.Now().Format(time.RFC3339)
	}
	n.LastHeartbeat = time.Now().Format(time.RFC3339)

	if err := h.db.RegisterNode(&n); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Node registered: %s (%s)", n.ID, n.TailscaleName)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, &n)
}

func (h *Handler) handleNodeHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathSegment(r.URL.Path, "/api/nodes/", "/heartbeat")
	}

	if err := h.db.UpdateNodeHeartbeat(id, time.Now().Format(time.RFC3339)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handler) handleNodeList(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.db.ListNodes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

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
	n, err := h.db.GetNode(id)
	if err != nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, n)
}

// --- VM handlers ---

type vmCreateRequest struct {
	Name      string   `json:"name"`
	VCPU      int      `json:"vcpu,omitempty"`
	RAMMIB    int      `json:"ram_mib,omitempty"`
	Disk      string   `json:"disk,omitempty"`
	CloneURL  string   `json:"clone_url,omitempty"`
	CloneURLs []string `json:"clone_urls,omitempty"`
	Mode      string   `json:"mode,omitempty"`
	NodeID    string   `json:"node_id,omitempty"` // optional: pin to specific node
}

func (h *Handler) handleVMCreate(w http.ResponseWriter, r *http.Request) {
	var req vmCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		// Generate a name
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

	// Check if VM already exists
	if _, err := h.db.GetVM(req.Name); err == nil {
		http.Error(w, fmt.Sprintf("VM '%s' already exists", req.Name), http.StatusConflict)
		return
	}

	// Get current SSH keys to pass to node
	sshKeys, _ := h.db.ListSSHKeys()

	// Build candidate node list
	var candidates []*db.Node
	if req.NodeID != "" {
		targetNode, err := h.db.GetNode(req.NodeID)
		if err != nil {
			http.Error(w, "specified node not found", http.StatusBadRequest)
			return
		}
		candidates = []*db.Node{targetNode}
	} else {
		nodes, err := h.db.ActiveNodes()
		if err != nil || len(nodes) == 0 {
			http.Error(w, "no active nodes", http.StatusServiceUnavailable)
			return
		}
		// Query real-time health and filter reachable nodes
		for _, n := range nodes {
			fc := node.NewFastClient(n.APIAddr)
			if health := fc.Health(); health != nil {
				n.RAMAllocatedMIB = health.RAMAllocatedMIB
				n.VMsRunning = health.VMsRunning
				n.RAMTotalMIB = health.RAMTotalMIB
				candidates = append(candidates, n)
			} else {
				log.Printf("Scheduling: node %s unreachable, skipping", n.ID)
			}
		}
		if len(candidates) == 0 {
			http.Error(w, "no reachable nodes", http.StatusServiceUnavailable)
			return
		}
		// Sort by most free RAM
		sorted, err := scheduler.PickNode(candidates, req.RAMMIB)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		// Put the best candidate first, keep the rest as fallbacks
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
			Name:           req.Name,
			VCPU:           req.VCPU,
			RAMMIB:         req.RAMMIB,
			Disk:           req.Disk,
			CloneURL:       req.CloneURL,
			CloneURLs:      req.CloneURLs,
			Mode:           req.Mode,
			AuthorizedKeys: sshKeys,
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
			// Mark node as down if it's a connection error
			h.db.SetNodeStatus(candidate.ID, "down")
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

	// Record in DB — only name, node, status
	h.db.CreateVM(&db.VM{
		Name:   nodeResp.Name,
		NodeID: targetNode.ID,
		Status: nodeResp.Status,
	})

	log.Printf("VM created: %s on node %s", nodeResp.Name, targetNode.ID)

	// Final ready event — include details from node response
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
	Name        string `json:"name"`
	NodeID      string `json:"node_id"`
	NodeName    string `json:"node_name"`
	TailscaleIP string `json:"tailscale_ip"`
	Mode        string `json:"mode"`
	VCPU        int    `json:"vcpu"`
	RAMMIB      int    `json:"ram_mib"`
	Disk        string `json:"disk"`
	Status      string `json:"status"`
}

func (h *Handler) handleVMList(w http.ResponseWriter, r *http.Request) {
	vms, err := h.db.ListVMs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Group VMs by node so we make one request per node
	nodeVMs := make(map[string][]*db.VM)
	for _, v := range vms {
		nodeVMs[v.NodeID] = append(nodeVMs[v.NodeID], v)
	}

	// Fetch details from each node in parallel
	type nodeResult struct {
		nodeID string
		vms    map[string]*node.VMDetail // name -> detail
	}
	results := make(chan nodeResult, len(nodeVMs))

	for nodeID := range nodeVMs {
		go func(nid string) {
			nr := nodeResult{nodeID: nid, vms: make(map[string]*node.VMDetail)}
			n, err := h.db.GetNode(nid)
			if err != nil || n.APIAddr == "" {
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

	// Collect results
	detailsByNode := make(map[string]map[string]*node.VMDetail)
	for range nodeVMs {
		nr := <-results
		detailsByNode[nr.nodeID] = nr.vms
	}

	// Build response
	var entries []vmListEntry
	for _, v := range vms {
		n, _ := h.db.GetNode(v.NodeID)
		nodeName := v.NodeID
		if n != nil {
			nodeName = n.TailscaleName
		}

		entry := vmListEntry{
			Name:     v.Name,
			NodeID:   v.NodeID,
			NodeName: nodeName,
			Status:   v.Status,
		}

		// Enrich with node detail if available
		if nodeDetail, ok := detailsByNode[v.NodeID]; ok {
			if detail, ok := nodeDetail[v.Name]; ok {
				entry.TailscaleIP = detail.TailscaleIP
				entry.Mode = detail.Mode
				entry.VCPU = detail.VCPU
				entry.RAMMIB = detail.RAMMIB
				entry.Disk = detail.Disk
				entry.Status = detail.Status // node's view is authoritative
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
	vm, err := h.db.GetVM(name)
	if err != nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	n, _ := h.db.GetNode(vm.NodeID)
	result := map[string]interface{}{
		"name":    vm.Name,
		"node_id": vm.NodeID,
		"status":  vm.Status,
	}
	if n != nil {
		result["node_name"] = n.TailscaleName

		// Try to get details from node
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

func (h *Handler) handleVMDestroy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/vms/")
	}

	vm, err := h.db.GetVM(name)
	if err != nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Mark as destroying first
	h.db.UpdateVMStatus(name, "destroying")

	// Tell node to destroy
	n, _ := h.db.GetNode(vm.NodeID)
	if n != nil {
		client := node.NewClient(n.APIAddr)
		if err := client.Destroy(name); err != nil {
			log.Printf("Node destroy failed for %s: %v (cleaning up DB record anyway)", name, err)
		}
	}

	// Always remove from DB — a stale record is worse than a phantom VM on a node,
	// and the health monitor's SyncNodeVMs will catch any real orphans.
	h.db.DeleteVM(name)
	log.Printf("VM destroyed: %s", name)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleVMStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractPathSegment(r.URL.Path, "/api/vms/", "/stop")
	}

	vm, err := h.db.GetVM(name)
	if err != nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	n, _ := h.db.GetNode(vm.NodeID)
	if n == nil {
		http.Error(w, "node not found", http.StatusInternalServerError)
		return
	}

	client := node.NewClient(n.APIAddr)
	if err := client.Stop(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.db.UpdateVMStatus(name, "stopped")
	writeJSON(w, map[string]string{"status": "stopped"})
}

func (h *Handler) handleVMStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractPathSegment(r.URL.Path, "/api/vms/", "/start")
	}

	vm, err := h.db.GetVM(name)
	if err != nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	n, _ := h.db.GetNode(vm.NodeID)
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

	h.db.UpdateVMStatus(name, "running")
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

	// Generate name if not provided
	if req.DstName == "" {
		out, err := exec.Command("boxcutter-names", "generate").Output()
		if err != nil {
			http.Error(w, "failed to generate name", http.StatusInternalServerError)
			return
		}
		req.DstName = strings.TrimSpace(string(out))
	}

	// Check if destination already exists
	if _, err := h.db.GetVM(req.DstName); err == nil {
		http.Error(w, fmt.Sprintf("VM '%s' already exists", req.DstName), http.StatusConflict)
		return
	}

	// Find the source VM's node
	srcVM, err := h.db.GetVM(srcName)
	if err != nil {
		http.Error(w, "source VM not found", http.StatusNotFound)
		return
	}

	n, _ := h.db.GetNode(srcVM.NodeID)
	if n == nil {
		http.Error(w, "source node not found", http.StatusInternalServerError)
		return
	}

	// Stream progress
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

	// Record new VM in DB
	h.db.CreateVM(&db.VM{
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
	vm, err := h.db.GetVM(name)
	if err != nil {
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
	n, _ := h.db.GetNode(vm.NodeID)
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
	vm, err := h.db.GetVM(name)
	if err != nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	n, _ := h.db.GetNode(vm.NodeID)
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
	vm, err := h.db.GetVM(name)
	if err != nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	n, _ := h.db.GetNode(vm.NodeID)
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

// --- Golden images ---

type goldenListEntry struct {
	Version  string   `json:"version"`
	Nodes    []string `json:"nodes"`
}

func (h *Handler) handleGoldenList(w http.ResponseWriter, r *http.Request) {
	images, err := h.db.ListGoldenImages()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Group by version
	versionNodes := make(map[string][]string)
	var order []string
	for _, img := range images {
		if _, seen := versionNodes[img.Version]; !seen {
			order = append(order, img.Version)
		}
		// Resolve node name
		nodeName := img.NodeID
		if n, err := h.db.GetNode(img.NodeID); err == nil {
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

	// Publish to MQTT so nodes get notified
	if h.mqtt != nil {
		if err := h.mqtt.PublishGoldenHead(req.Version); err != nil {
			log.Printf("mqtt: failed to publish golden head: %v", err)
		}
	}

	log.Printf("Golden head set to %s", req.Version)
	writeJSON(w, map[string]string{"version": req.Version, "status": "ok"})
}

// --- Migration (orchestrator self-migration) ---

// handleMigrate is called on the NEW orchestrator by the control plane.
// It drives the entire migration from the old orchestrator.
func (h *Handler) handleMigrate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceAddr string `json:"source_addr"` // e.g., "192.168.50.2:8801"
		SourceIP   string `json:"source_ip"`   // e.g., "192.168.50.2"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.SourceAddr == "" || req.SourceIP == "" {
		http.Error(w, "source_addr and source_ip are required", http.StatusBadRequest)
		return
	}

	log.Printf("Migration: starting from source %s", req.SourceAddr)

	sourceURL := "http://" + req.SourceAddr
	client := &http.Client{Timeout: 30 * time.Second}

	// 1. Tell old orchestrator to prepare for migration
	log.Printf("Migration: sending prepare-migrate to %s", req.SourceAddr)
	resp, err := client.Post(sourceURL+"/api/prepare-migrate", "application/json", nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("prepare-migrate failed: %v", err), http.StatusBadGateway)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("prepare-migrate returned %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	// 2. rsync the database from old orchestrator
	log.Printf("Migration: rsyncing database from %s", req.SourceIP)
	dbPath := "/var/lib/boxcutter/orchestrator.db"

	sshOpts := "ssh -i /etc/boxcutter/secrets/cluster-ssh.key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
	rsyncCmd := exec.Command("rsync", "-az", "--timeout=30", "-e", sshOpts,
		fmt.Sprintf("ubuntu@%s:%s", req.SourceIP, dbPath),
		dbPath+".migrated")
	rsyncCmd.Stdout = os.Stdout
	rsyncCmd.Stderr = os.Stderr
	if err := rsyncCmd.Run(); err != nil {
		http.Error(w, fmt.Sprintf("rsync db failed: %v", err), http.StatusInternalServerError)
		return
	}

	// Also rsync Tailscale state to preserve identity
	log.Printf("Migration: rsyncing Tailscale state from %s", req.SourceIP)
	rsyncTS := exec.Command("rsync", "-az", "--timeout=30", "-e", sshOpts,
		fmt.Sprintf("ubuntu@%s:/var/lib/tailscale/", req.SourceIP),
		"/var/lib/tailscale/")
	rsyncTS.Stdout = os.Stdout
	rsyncTS.Stderr = os.Stderr
	if err := rsyncTS.Run(); err != nil {
		log.Printf("Migration: tailscale rsync failed (non-fatal): %v", err)
	}

	// 3. Tell old orchestrator to shut down
	log.Printf("Migration: sending shutdown to %s", req.SourceAddr)
	resp, err = client.Post(sourceURL+"/api/shutdown", "application/json", nil)
	if err != nil {
		log.Printf("Migration: shutdown request failed (continuing): %v", err)
	} else {
		resp.Body.Close()
	}

	// 4. Wait for old orchestrator to become unreachable
	log.Printf("Migration: waiting for old orchestrator to stop...")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(sourceURL + "/healthz")
		if err != nil {
			break // unreachable = good
		}
		resp.Body.Close()
		time.Sleep(2 * time.Second)
	}

	// 5. Activate the migrated database
	log.Printf("Migration: activating migrated database")
	os.Rename(dbPath+".migrated", dbPath)

	// 6. Start Tailscale with the old identity
	log.Printf("Migration: starting Tailscale")
	exec.Command("systemctl", "restart", "tailscaled").Run()
	time.Sleep(3 * time.Second)
	exec.Command("tailscale", "up").Run()

	// 7. Restart our own orchestrator service to pick up the new DB
	log.Printf("Migration: restarting orchestrator service")
	exec.Command("systemctl", "restart", "boxcutter-orchestrator").Run()

	log.Printf("Migration: complete")
	writeJSON(w, map[string]string{"status": "migrated"})
}

// handlePrepareMigrate is called on the OLD orchestrator by the new one.
// It stops accepting new work and prepares for state transfer.
func (h *Handler) handlePrepareMigrate(w http.ResponseWriter, r *http.Request) {
	const migrationTimeout = 5 * time.Minute

	h.migrateMu.Lock()
	h.migrating = true
	h.migrateDeadline = time.Now().Add(migrationTimeout)
	h.migrateMu.Unlock()

	log.Printf("Prepare-migrate: entering migration mode (expires in %v), new requests will be rejected", migrationTimeout)

	writeJSON(w, map[string]string{
		"status":  "ready",
		"db_path": "/var/lib/boxcutter/orchestrator.db",
	})
}

// handleShutdown is called on the OLD orchestrator by the new one.
// It gracefully shuts down the VM.
func (h *Handler) handleShutdown(w http.ResponseWriter, r *http.Request) {
	log.Printf("Shutdown: received shutdown request, powering off in 2 seconds")
	writeJSON(w, map[string]string{"status": "shutting_down"})

	go func() {
		time.Sleep(2 * time.Second)
		// Release Tailscale hostname so the replacement VM can claim it
		exec.Command("tailscale", "logout").Run()
		time.Sleep(1 * time.Second)
		exec.Command("shutdown", "-h", "now").Run()
	}()
}

// --- Health ---

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	nodes, _ := h.db.ListNodes()
	vms, _ := h.db.ListVMs()

	var activeNodes int
	var totalRAM, allocRAM int

	// Query real-time health from active nodes
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

// --- Wingman handlers ---

type wingmanActivityEntry struct {
	Name        string                       `json:"name"`
	NodeID      string                       `json:"node_id"`
	NodeName    string                       `json:"node_name"`
	VMStatus    string                       `json:"vm_status"`
	Activity    *node.WingmanActivityReport  `json:"activity,omitempty"`
	PendingMsgs int                          `json:"pending_messages"`
}

func (h *Handler) handleWingmanActivity(w http.ResponseWriter, r *http.Request) {
	vms, err := h.db.ListVMs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Group VMs by node
	nodeVMs := make(map[string][]*db.VM)
	for _, v := range vms {
		nodeVMs[v.NodeID] = append(nodeVMs[v.NodeID], v)
	}

	// Fan out to each node
	type nodeResult struct {
		nodeID   string
		activity map[string]*node.WingmanVMActivity // vmid -> activity
	}
	results := make(chan nodeResult, len(nodeVMs))

	for nodeID := range nodeVMs {
		go func(nid string) {
			nr := nodeResult{nodeID: nid, activity: make(map[string]*node.WingmanVMActivity)}
			n, err := h.db.GetNode(nid)
			if err != nil || n.APIAddr == "" {
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

	// Collect
	activityByNode := make(map[string]map[string]*node.WingmanVMActivity)
	for range nodeVMs {
		nr := <-results
		activityByNode[nr.nodeID] = nr.activity
	}

	// Build response
	var entries []wingmanActivityEntry
	for _, v := range vms {
		n, _ := h.db.GetNode(v.NodeID)
		nodeName := v.NodeID
		if n != nil {
			nodeName = n.TailscaleName
		}

		entry := wingmanActivityEntry{
			Name:     v.Name,
			NodeID:   v.NodeID,
			NodeName: nodeName,
			VMStatus: v.Status,
		}

		if nodeAct, ok := activityByNode[v.NodeID]; ok {
			if act, ok := nodeAct[v.Name]; ok {
				entry.Activity = act.LastActivity
				entry.PendingMsgs = act.PendingMessages
			}
		}

		entries = append(entries, entry)
	}

	writeJSON(w, entries)
}

func (h *Handler) handleWingmanVMActivity(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/wingman/activity/")
	}

	v, err := h.db.GetVM(name)
	if err != nil {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}

	n, err := h.db.GetNode(v.NodeID)
	if err != nil || n.APIAddr == "" {
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
			writeJSON(w, wingmanActivityEntry{
				Name:        v.Name,
				NodeID:      v.NodeID,
				NodeName:    nodeName,
				VMStatus:    v.Status,
				Activity:    act.LastActivity,
				PendingMsgs: act.PendingMessages,
			})
			return
		}
	}

	// VM exists but no activity data yet
	nodeName := v.NodeID
	if n != nil {
		nodeName = n.TailscaleName
	}
	writeJSON(w, wingmanActivityEntry{
		Name:     v.Name,
		NodeID:   v.NodeID,
		NodeName: nodeName,
		VMStatus: v.Status,
	})
}

func (h *Handler) handleWingmanMessage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/wingman/message/")
	}

	var msg node.WingmanMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if msg.ID == "" {
		msg.ID = fmt.Sprintf("wm-%d", time.Now().UnixNano())
	}
	if msg.CreatedAt == "" {
		msg.CreatedAt = time.Now().Format(time.RFC3339)
	}

	v, err := h.db.GetVM(name)
	if err != nil {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}

	n, err := h.db.GetNode(v.NodeID)
	if err != nil || n.APIAddr == "" {
		http.Error(w, "node not available", http.StatusServiceUnavailable)
		return
	}

	nc := node.NewClient(n.APIAddr)
	if err := nc.SendWingmanMessage(name, &msg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"status": "sent", "message_id": msg.ID})
}

func (h *Handler) handleWingmanBroadcast(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body     string `json:"body"`
		From     string `json:"from"`
		Priority string `json:"priority"`
		SendKeys bool   `json:"send_keys,omitempty"`
		Filter   string `json:"filter,omitempty"` // optional: "running", "stopped"
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

	vms, err := h.db.ListVMs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var sent []string
	var failed []string

	for _, v := range vms {
		if req.Filter != "" && v.Status != req.Filter {
			continue
		}

		n, err := h.db.GetNode(v.NodeID)
		if err != nil || n.APIAddr == "" {
			failed = append(failed, v.Name)
			continue
		}

		msg := &node.WingmanMessage{
			ID:        fmt.Sprintf("wm-%d-%s", time.Now().UnixNano(), v.Name),
			From:      req.From,
			Body:      req.Body,
			Priority:  req.Priority,
			SendKeys:  req.SendKeys,
			CreatedAt: time.Now().Format(time.RFC3339),
		}

		nc := node.NewClient(n.APIAddr)
		if err := nc.SendWingmanMessage(v.Name, msg); err != nil {
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
