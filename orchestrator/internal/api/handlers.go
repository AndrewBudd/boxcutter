package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/node"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/scheduler"
)

type Handler struct {
	db *db.DB
}

func NewHandler(database *db.DB) *Handler {
	h := &Handler{db: database}
	go h.healthMonitorLoop()
	return h
}

// healthMonitorLoop checks node health every 30 seconds and marks nodes down/up.
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
			} else {
				if n.Status == "down" {
					log.Printf("Node %s: back up, marking active", n.ID)
					h.db.SetNodeStatus(n.ID, "active")
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
	mux.HandleFunc("DELETE /api/nodes/{id}", h.handleNodeDelete)
	mux.HandleFunc("POST /api/nodes/{id}/drain", h.handleNodeDrain)

	// VM management
	mux.HandleFunc("POST /api/vms", h.handleVMCreate)
	mux.HandleFunc("GET /api/vms", h.handleVMList)
	mux.HandleFunc("GET /api/vms/{name}", h.handleVMGet)
	mux.HandleFunc("DELETE /api/vms/{name}", h.handleVMDestroy)
	mux.HandleFunc("POST /api/vms/{name}/stop", h.handleVMStop)
	mux.HandleFunc("POST /api/vms/{name}/start", h.handleVMStart)

	// Migration
	mux.HandleFunc("POST /api/migrate", h.handleMigrate)

	// SSH keys
	mux.HandleFunc("POST /api/keys/add", h.handleAddKeys)
	mux.HandleFunc("GET /api/keys", h.handleListKeys)
	mux.HandleFunc("DELETE /api/keys/{user}", h.handleDeleteKeys)

	// Health
	mux.HandleFunc("GET /api/health", h.handleHealth)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
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

	// Check golden image in background — build if missing
	if n.APIAddr != "" {
		go func() {
			client := node.NewClient(n.APIAddr)
			health, err := client.Health()
			if err != nil || health.GoldenReady {
				return
			}
			log.Printf("Node %s: golden image missing, starting build...", n.ID)
			h.db.SetNodeStatus(n.ID, "provisioning")
			err = client.BuildGolden(func(phase, msg string) {})
			if err != nil {
				log.Printf("Node %s: golden build failed: %v", n.ID, err)
				return
			}
			h.db.SetNodeStatus(n.ID, "active")
			log.Printf("Node %s: golden image ready", n.ID)
		}()
	}
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

func (h *Handler) handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractName(r.URL.Path, "/api/nodes/")
	}
	if err := h.db.DeleteNode(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleNodeDrain(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathSegment(r.URL.Path, "/api/nodes/", "/drain")
	}

	// Mark node as draining
	if err := h.db.SetNodeStatus(id, "draining"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Get VMs on this node
	vms, err := h.db.ListVMsByNode(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Get target nodes with real-time health
	allNodes, _ := h.db.ActiveNodes()
	for _, n := range allNodes {
		fc := node.NewFastClient(n.APIAddr)
		if health := fc.Health(); health != nil {
			// Store in a way scheduler can use
			// We need RAM info for scheduling — fetch it
		}
	}

	// Migrate each VM
	var migrated, failed int
	for _, vm := range vms {
		// For drain, we need to pick a target — query real-time health
		targetNode, err := h.pickNodeForMigration(allNodes, id)
		if err != nil {
			log.Printf("Drain: no target for %s: %v", vm.Name, err)
			failed++
			continue
		}

		if err := h.migrateVM(vm.Name, id, targetNode.ID); err != nil {
			log.Printf("Drain: failed to migrate %s: %v", vm.Name, err)
			failed++
		} else {
			migrated++
		}
	}

	if failed == 0 && len(vms) > 0 {
		h.db.SetNodeStatus(id, "retired")
	}

	writeJSON(w, map[string]interface{}{
		"node":     id,
		"total":    len(vms),
		"migrated": migrated,
		"failed":   failed,
		"status":   "draining",
	})
}

// pickNodeForMigration picks a target node, excluding the given node ID.
func (h *Handler) pickNodeForMigration(nodes []*db.Node, excludeID string) (*db.Node, error) {
	var candidates []*db.Node
	for _, n := range nodes {
		if n.ID != excludeID {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no target nodes available")
	}
	// Simple: pick the first available (scheduler needs health data, but for drain just pick any)
	return candidates[0], nil
}

// --- VM handlers ---

type vmCreateRequest struct {
	Name     string `json:"name"`
	VCPU     int    `json:"vcpu,omitempty"`
	RAMMIB   int    `json:"ram_mib,omitempty"`
	Disk     string `json:"disk,omitempty"`
	CloneURL string `json:"clone_url,omitempty"`
	Mode     string `json:"mode,omitempty"`
	NodeID   string `json:"node_id,omitempty"` // optional: pin to specific node
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
			log.Printf("Node destroy failed for %s: %v (marked destroying, will retry)", name, err)
			http.Error(w, fmt.Sprintf("destroy in progress but node error: %v", err), http.StatusAccepted)
			return
		}
	}

	// Confirmed destroyed on node — remove from DB
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

// --- Migration ---

type migrateRequest struct {
	VMName   string `json:"vm"`
	ToNodeID string `json:"to"`
}

func (h *Handler) handleMigrate(w http.ResponseWriter, r *http.Request) {
	var req migrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	vm, err := h.db.GetVM(req.VMName)
	if err != nil {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	toNodeID := req.ToNodeID
	if toNodeID == "" {
		// Auto-pick
		nodes, _ := h.db.ActiveNodes()
		var candidates []*db.Node
		for _, n := range nodes {
			if n.ID != vm.NodeID {
				candidates = append(candidates, n)
			}
		}
		if len(candidates) == 0 {
			http.Error(w, "no target nodes available", http.StatusServiceUnavailable)
			return
		}
		toNodeID = candidates[0].ID
	}

	if err := h.migrateVM(req.VMName, vm.NodeID, toNodeID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{
		"vm":        req.VMName,
		"from_node": vm.NodeID,
		"to_node":   toNodeID,
		"status":    "migrated",
	})
}

func (h *Handler) migrateVM(vmName, fromNodeID, toNodeID string) error {
	fromNode, err := h.db.GetNode(fromNodeID)
	if err != nil {
		return fmt.Errorf("source node not found: %w", err)
	}
	toNode, err := h.db.GetNode(toNodeID)
	if err != nil {
		return fmt.Errorf("target node not found: %w", err)
	}

	log.Printf("Migrating %s: %s → %s", vmName, fromNode.TailscaleName, toNode.TailscaleName)

	// Mark as migrating
	h.db.UpdateVMStatus(vmName, "migrating")

	// Tell the source node to migrate the VM directly to the target node.
	srcClient := node.NewClient(fromNode.APIAddr)
	_, err = srcClient.Migrate(vmName, &node.MigrateRequest{
		TargetAddr:     toNode.APIAddr,
		TargetBridgeIP: toNode.BridgeIP,
	})
	if err != nil {
		h.db.UpdateVMStatus(vmName, "running") // revert on failure
		return fmt.Errorf("migration failed: %w", err)
	}

	// Update DB with new node
	h.db.UpdateVMNode(vmName, toNodeID)
	h.db.UpdateVMStatus(vmName, "running")

	log.Printf("Migration complete: %s → %s", vmName, toNode.TailscaleName)
	return nil
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
