package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/node"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/scheduler"
)

type Handler struct {
	db *db.DB
}

func NewHandler(database *db.DB) *Handler {
	return &Handler{db: database}
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
}

func (h *Handler) handleNodeHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathSegment(r.URL.Path, "/api/nodes/", "/heartbeat")
	}

	var req struct {
		RAMAllocatedMIB int `json:"ram_allocated_mib"`
		VMsRunning      int `json:"vms_running"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if err := h.db.UpdateNodeHeartbeat(id, req.RAMAllocatedMIB, req.VMsRunning, time.Now().Format(time.RFC3339)); err != nil {
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

	// Get target nodes
	allNodes, _ := h.db.ActiveNodes()

	// Migrate each VM
	var migrated, failed int
	for _, vm := range vms {
		targetNode, err := scheduler.PickNode(allNodes, vm.RAMMIB)
		if err != nil || targetNode.ID == id {
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
	if req.RAMMIB == 0 {
		req.RAMMIB = 8192
	}

	// Check if VM already exists
	if _, err := h.db.GetVM(req.Name); err == nil {
		http.Error(w, fmt.Sprintf("VM '%s' already exists", req.Name), http.StatusConflict)
		return
	}

	// Pick a node
	var targetNode *db.Node
	var err error

	if req.NodeID != "" {
		targetNode, err = h.db.GetNode(req.NodeID)
		if err != nil {
			http.Error(w, "specified node not found", http.StatusBadRequest)
			return
		}
	} else {
		nodes, err := h.db.ActiveNodes()
		if err != nil || len(nodes) == 0 {
			http.Error(w, "no active nodes", http.StatusServiceUnavailable)
			return
		}
		targetNode, err = scheduler.PickNode(nodes, req.RAMMIB)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}

	// Get current SSH keys to pass to node
	sshKeys, _ := h.db.ListSSHKeys()

	// Call node agent
	client := node.NewClient(targetNode.APIAddr)
	nodeResp, err := client.Create(&node.CreateRequest{
		Name:           req.Name,
		VCPU:           req.VCPU,
		RAMMIB:         req.RAMMIB,
		Disk:           req.Disk,
		CloneURL:       req.CloneURL,
		Mode:           req.Mode,
		AuthorizedKeys: sshKeys,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("node create failed: %v", err), http.StatusInternalServerError)
		return
	}

	// Record in DB
	vm := &db.VM{
		Name:        nodeResp.Name,
		NodeID:      targetNode.ID,
		TailscaleIP: nodeResp.TailscaleIP,
		Mark:        nodeResp.Mark,
		Mode:        nodeResp.Mode,
		VCPU:        req.VCPU,
		RAMMIB:      req.RAMMIB,
		Disk:        req.Disk,
		CloneURL:    req.CloneURL,
		Status:      nodeResp.Status,
		CreatedAt:   time.Now().Format(time.RFC3339),
	}
	h.db.CreateVM(vm)

	log.Printf("VM created: %s on node %s (Tailscale: %s)", vm.Name, targetNode.ID, vm.TailscaleIP)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]interface{}{
		"name":         vm.Name,
		"node":         targetNode.TailscaleName,
		"tailscale_ip": vm.TailscaleIP,
		"mode":         vm.Mode,
		"status":       vm.Status,
	})
}

func (h *Handler) handleVMList(w http.ResponseWriter, r *http.Request) {
	vms, err := h.db.ListVMs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Enrich with node name
	type vmEntry struct {
		*db.VM
		NodeName string `json:"node_name"`
	}
	var result []vmEntry
	for _, v := range vms {
		n, _ := h.db.GetNode(v.NodeID)
		nodeName := v.NodeID
		if n != nil {
			nodeName = n.TailscaleName
		}
		result = append(result, vmEntry{VM: v, NodeName: nodeName})
	}
	writeJSON(w, result)
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
	writeJSON(w, map[string]interface{}{
		"vm":   vm,
		"node": n,
	})
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

	// Tell node to destroy
	n, _ := h.db.GetNode(vm.NodeID)
	if n != nil {
		client := node.NewClient(n.APIAddr)
		if err := client.Destroy(name); err != nil {
			log.Printf("Node destroy warning: %v", err)
		}
	}

	// Remove from DB
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

	h.db.UpdateVM(name, vm.TailscaleIP, vm.Mark, "stopped")
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

	h.db.UpdateVM(name, resp.TailscaleIP, resp.Mark, "running")
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
		target, err := scheduler.PickNode(candidates, vm.RAMMIB)
		if err != nil {
			http.Error(w, "no suitable target node", http.StatusServiceUnavailable)
			return
		}
		toNodeID = target.ID
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

	// 1. Export from source
	srcClient := node.NewClient(fromNode.APIAddr)
	exportResp, err := srcClient.Export(vmName)
	if err != nil {
		return fmt.Errorf("export failed: %w", err)
	}

	// 2. Transfer COW via rsync over Tailscale
	srcCowPath := exportResp.CowPath
	dstCowDir := fmt.Sprintf("/var/lib/boxcutter/vms/%s/", vmName)

	// Ensure target dir exists via ssh
	rsyncDest := fmt.Sprintf("%s:%s", toNode.TailscaleIP, dstCowDir)

	log.Printf("Transferring COW image (%d bytes)...", exportResp.CowBytes)
	cmd := exec.Command("ssh", toNode.TailscaleIP, "mkdir", "-p", dstCowDir)
	cmd.Run()

	cmd = exec.Command("rsync", "--sparse", "-e", "ssh",
		srcCowPath, rsyncDest+"cow.img")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync failed: %s: %w", string(out), err)
	}

	// 3. Import on target
	dstClient := node.NewClient(toNode.APIAddr)
	importResp, err := dstClient.Import(vmName, exportResp.VMState)
	if err != nil {
		return fmt.Errorf("import failed: %w", err)
	}

	// 4. Update DB
	h.db.UpdateVMNode(vmName, toNodeID, importResp.Mark)
	h.db.UpdateVM(vmName, importResp.TailscaleIP, importResp.Mark, "running")

	// 5. Clean up source
	srcClient.Destroy(vmName)

	log.Printf("Migration complete: %s → %s (Tailscale: %s)", vmName, toNode.TailscaleName, importResp.TailscaleIP)
	return nil
}

// --- Health ---

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	nodes, _ := h.db.ListNodes()
	vms, _ := h.db.ListVMs()

	var activeNodes, totalRAM, allocRAM int
	for _, n := range nodes {
		if n.Status == "active" {
			activeNodes++
			totalRAM += n.RAMTotalMIB
			allocRAM += n.RAMAllocatedMIB
		}
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
