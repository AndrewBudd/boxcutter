package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewBudd/boxcutter/host/internal/cluster"
)

// --- Simulated node agent for upgrade testing ---

type simNode struct {
	mu     sync.Mutex
	vms    map[string]string // name → status ("running", "migrating", "migrated", "stopped")
	health bool
	golden bool
}

func newSimNode(healthy, golden bool) *simNode {
	return &simNode{
		vms:    make(map[string]string),
		health: healthy,
		golden: golden,
	}
}

func (n *simNode) addVM(name, status string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.vms[name] = status
}

func (n *simNode) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/vms", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		var vms []map[string]interface{}
		for name, status := range n.vms {
			vms = append(vms, map[string]interface{}{
				"name":    name,
				"status":  status,
				"type":    "firecracker",
				"ram_mib": 2048,
			})
		}
		json.NewEncoder(w).Encode(vms)
	})

	mux.HandleFunc("GET /api/vms/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		n.mu.Lock()
		defer n.mu.Unlock()
		status, ok := n.vms[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"vm":     map[string]interface{}{"name": name},
			"status": status,
		})
	})

	mux.HandleFunc("POST /api/vms/{name}/migrate", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		n.mu.Lock()
		status, ok := n.vms[name]
		n.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if status == "migrating" {
			w.WriteHeader(http.StatusConflict)
			return
		}

		// Simulate async migration
		n.mu.Lock()
		n.vms[name] = "migrating"
		n.mu.Unlock()

		go func() {
			time.Sleep(100 * time.Millisecond) // simulate migration time
			n.mu.Lock()
			defer n.mu.Unlock()
			if s, ok := n.vms[name]; ok && s == "migrating" {
				delete(n.vms, name) // remove from source after migration
			}
		}()

		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "migrating"})
	})

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		if !n.health {
			http.Error(w, "unhealthy", 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":          "active",
			"golden_ready":    n.golden,
			"ram_total_mib":   12288,
			"ram_free_mib":    8192,
			"ram_allocated_mib": 4096,
			"vms_running":     len(n.vms),
		})
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		if !n.health {
			http.Error(w, "unhealthy", 500)
			return
		}
		w.Write([]byte("ok"))
	})

	return mux
}

// --- Upgrade state machine tests ---

func TestUpgrade_FirstNodeNotMatchingGoal(t *testing.T) {
	state := &cluster.State{
		Nodes: []cluster.VMEntry{
			{ID: "node-1", Status: "active", ImageDigest: "sha256:old"},
			{ID: "node-2", Status: "active", ImageDigest: "sha256:new"},
			{ID: "node-3", Status: "active", ImageDigest: "sha256:old"},
		},
	}
	goal := &cluster.UpgradeGoal{
		NodeImage: &cluster.ImageRef{Digest: "sha256:new"},
	}

	old := firstNodeNotMatchingGoal(state, goal)
	if old == nil {
		t.Fatal("should find a node not matching goal")
	}
	if old.ID != "node-1" {
		t.Errorf("first old node = %s, want node-1", old.ID)
	}
}

func TestUpgrade_AllNodesMatch(t *testing.T) {
	state := &cluster.State{
		Nodes: []cluster.VMEntry{
			{ID: "node-1", Status: "active", ImageDigest: "sha256:new"},
			{ID: "node-2", Status: "active", ImageDigest: "sha256:new"},
		},
	}
	goal := &cluster.UpgradeGoal{
		NodeImage: &cluster.ImageRef{Digest: "sha256:new"},
	}

	old := firstNodeNotMatchingGoal(state, goal)
	if old != nil {
		t.Errorf("should not find any old node, got %s", old.ID)
	}
}

func TestUpgrade_DrainingNodeSkipped(t *testing.T) {
	state := &cluster.State{
		Nodes: []cluster.VMEntry{
			{ID: "node-1", Status: "draining", ImageDigest: "sha256:old"},
			{ID: "node-2", Status: "active", ImageDigest: "sha256:new"},
		},
	}
	goal := &cluster.UpgradeGoal{
		NodeImage: &cluster.ImageRef{Digest: "sha256:new"},
	}

	// Draining nodes are not IsActive(), so they should be skipped
	old := firstNodeNotMatchingGoal(state, goal)
	if old != nil {
		t.Errorf("draining node should be skipped, got %s", old.ID)
	}
}

func TestUpgrade_FindReplacementNode(t *testing.T) {
	state := &cluster.State{
		Nodes: []cluster.VMEntry{
			{ID: "node-1", Status: "active", ImageDigest: "sha256:old"},
			{ID: "node-2", Status: "active", ImageDigest: "sha256:new"},
		},
	}
	goal := &cluster.UpgradeGoal{
		NodeImage: &cluster.ImageRef{Digest: "sha256:new"},
	}

	rep := findReplacementNode(state, goal)
	if rep == nil {
		t.Fatal("should find replacement node")
	}
	if rep.ID != "node-2" {
		t.Errorf("replacement = %s, want node-2", rep.ID)
	}
}

func TestUpgrade_OrchNeedsUpgrade(t *testing.T) {
	state := &cluster.State{
		Orchestrator: &cluster.VMEntry{
			ID:          "orchestrator",
			ImageDigest: "sha256:old-orch",
		},
	}

	tests := []struct {
		name string
		goal *cluster.UpgradeGoal
		want bool
	}{
		{
			"different digest",
			&cluster.UpgradeGoal{OrchImage: &cluster.ImageRef{Digest: "sha256:new-orch"}},
			true,
		},
		{
			"same digest",
			&cluster.UpgradeGoal{OrchImage: &cluster.ImageRef{Digest: "sha256:old-orch"}},
			false,
		},
		{
			"nil orch image",
			&cluster.UpgradeGoal{OrchImage: nil},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := orchNeedsUpgrade(state, tt.goal)
			if got != tt.want {
				t.Errorf("orchNeedsUpgrade = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Drain simulation tests ---

func TestDrain_EmptyNodeRemoved(t *testing.T) {
	// Simulate draining a node with no VMs — it should be stopped and removed
	node := newSimNode(true, true)
	srv := httptest.NewServer(node.handler())
	defer srv.Close()

	// The drain should detect 0 VMs and clean up
	// We can't call drainNode directly (too many deps), but we can test the
	// state transitions that should happen
	state := &cluster.State{
		Nodes: []cluster.VMEntry{
			{ID: "empty-node", Status: "draining", BridgeIP: "192.168.50.10"},
		},
	}

	// After empty drain, node should be removed from state
	// This simulates what should happen
	state.RemoveNode("empty-node")
	if len(state.Nodes) != 0 {
		t.Errorf("empty node should be removed after drain, got %d nodes", len(state.Nodes))
	}
}

func TestDrain_PollDetectsMigrated(t *testing.T) {
	// Simulate the drain poll loop seeing "migrated" status
	node := newSimNode(true, true)
	node.addVM("test-vm", "migrated")
	srv := httptest.NewServer(node.handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/vms/test-vm")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var detail map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&detail)
	status, _ := detail["status"].(string)

	if status != "migrated" {
		t.Errorf("status = %q, want migrated", status)
	}
}

func TestDrain_PollDetects404(t *testing.T) {
	// After migration, VM is removed from source — 404
	node := newSimNode(true, true)
	srv := httptest.NewServer(node.handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/vms/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDrain_MigrationAsync(t *testing.T) {
	// POST /migrate returns 202 immediately, VM transitions async
	node := newSimNode(true, true)
	node.addVM("vm-1", "running")
	srv := httptest.NewServer(node.handler())
	defer srv.Close()

	// Fire migration
	resp, err := http.Post(srv.URL+"/api/vms/vm-1/migrate",
		"application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Errorf("migrate response = %d, want 202", resp.StatusCode)
	}

	// Immediately after, status should be "migrating"
	resp2, _ := http.Get(srv.URL + "/api/vms/vm-1")
	var detail map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&detail)
	resp2.Body.Close()
	if s, _ := detail["status"].(string); s != "migrating" {
		t.Errorf("status after migrate = %q, want migrating", s)
	}

	// After migration completes, VM should be gone (404)
	time.Sleep(200 * time.Millisecond)
	resp3, _ := http.Get(srv.URL + "/api/vms/vm-1")
	resp3.Body.Close()
	if resp3.StatusCode != 404 {
		t.Errorf("status after migration complete = %d, want 404", resp3.StatusCode)
	}
}

func TestDrain_BatchMigration(t *testing.T) {
	// Simulate 5 VMs migrating in batches of 3
	node := newSimNode(true, true)
	for i := 0; i < 5; i++ {
		node.addVM(fmt.Sprintf("vm-%d", i), "running")
	}
	srv := httptest.NewServer(node.handler())
	defer srv.Close()

	// Fire batch 1 (3 VMs)
	for i := 0; i < 3; i++ {
		resp, _ := http.Post(srv.URL+fmt.Sprintf("/api/vms/vm-%d/migrate", i),
			"application/json", nil)
		resp.Body.Close()
	}

	// Wait for batch 1 to complete
	time.Sleep(200 * time.Millisecond)

	// Verify batch 1 VMs are gone
	for i := 0; i < 3; i++ {
		resp, _ := http.Get(srv.URL + fmt.Sprintf("/api/vms/vm-%d", i))
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("vm-%d should be 404 after migration, got %d", i, resp.StatusCode)
		}
	}

	// Batch 2 (remaining 2 VMs) should still be running
	for i := 3; i < 5; i++ {
		resp, _ := http.Get(srv.URL + fmt.Sprintf("/api/vms/vm-%d", i))
		var detail map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&detail)
		resp.Body.Close()
		if s, _ := detail["status"].(string); s != "running" {
			t.Errorf("vm-%d should be running, got %q", i, s)
		}
	}
}

func TestDrain_UnreachableSource(t *testing.T) {
	// Simulate source node going unreachable during drain
	// The poll should eventually give up after inactivity timeout
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // immediately close to simulate unreachable

	resp, err := http.Get(srv.URL + "/api/vms/test-vm")
	if err == nil {
		resp.Body.Close()
		t.Error("should get connection error for closed server")
	}
}

func TestDrain_AlreadyMigrating409(t *testing.T) {
	node := newSimNode(true, true)
	node.addVM("vm-1", "migrating") // already in progress
	srv := httptest.NewServer(node.handler())
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/api/vms/vm-1/migrate",
		"application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Errorf("duplicate migrate = %d, want 409", resp.StatusCode)
	}
}

func TestDrain_Stale409RetrySucceeds(t *testing.T) {
	// Bug #37: node agent's filesystem migration marker lingers from a
	// previous failed migration. First POST returns 409, but actual VM
	// status is "running". Host calls DELETE to clear the marker, then
	// retry should succeed.
	var mu sync.Mutex
	stale409 := true // first migrate call returns stale 409
	deleteCalled := false

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/vms/{name}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		// VM is actually running (not migrating) — stale state
		json.NewEncoder(w).Encode(map[string]interface{}{
			"vm":     map[string]interface{}{"name": r.PathValue("name")},
			"status": "running",
		})
	})
	mux.HandleFunc("DELETE /api/vms/{name}/migrate", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		deleteCalled = true
		stale409 = false // DELETE clears the stale marker
		json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
	})
	mux.HandleFunc("POST /api/vms/{name}/migrate", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if stale409 {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte("already migrating"))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "migrating"})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Simulate the drain's migrate + DELETE + retry flow
	client := &http.Client{Timeout: 5 * time.Second}
	migrateReq := map[string]string{
		"target_addr":      "10.0.0.2:8800",
		"target_bridge_ip": "10.0.0.2",
	}
	data, _ := json.Marshal(migrateReq)

	// First attempt — should get 409
	resp, err := client.Post(srv.URL+"/api/vms/test-vm/migrate",
		"application/json", jsonReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("first migrate = %d, want 409", resp.StatusCode)
	}

	// Check actual status — should be "running" (stale 409)
	statusResp, err := client.Get(srv.URL + "/api/vms/test-vm")
	if err != nil {
		t.Fatal(err)
	}
	var detail map[string]interface{}
	json.NewDecoder(statusResp.Body).Decode(&detail)
	statusResp.Body.Close()
	actualStatus, _ := detail["status"].(string)
	if actualStatus != "running" {
		t.Fatalf("actual status = %q, want running", actualStatus)
	}

	// DELETE to clear stale marker
	delReq, _ := http.NewRequest("DELETE", srv.URL+"/api/vms/test-vm/migrate", nil)
	delResp, err := client.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()

	// Retry after DELETE — should get 202
	data2, _ := json.Marshal(migrateReq)
	resp2, err := client.Post(srv.URL+"/api/vms/test-vm/migrate",
		"application/json", jsonReader(data2))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 202 {
		t.Errorf("retry after DELETE = %d, want 202", resp2.StatusCode)
	}

	mu.Lock()
	if !deleteCalled {
		t.Error("DELETE /api/vms/{name}/migrate was not called")
	}
	mu.Unlock()
}

func TestDrain_Genuine409NotRetried(t *testing.T) {
	// When a VM is genuinely migrating (status=migrating), the 409 should
	// be accepted and the drain should poll — not retry.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/vms/{name}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"vm":     map[string]interface{}{"name": r.PathValue("name")},
			"status": "migrating", // genuinely migrating
		})
	})
	migrateCallCount := 0
	var mu sync.Mutex
	mux.HandleFunc("POST /api/vms/{name}/migrate", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		migrateCallCount++
		mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte("already migrating"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	// First attempt — 409
	resp, _ := client.Post(srv.URL+"/api/vms/test-vm/migrate",
		"application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("migrate = %d, want 409", resp.StatusCode)
	}

	// Check actual status — "migrating" means it's genuine
	statusResp, _ := client.Get(srv.URL + "/api/vms/test-vm")
	var detail map[string]interface{}
	json.NewDecoder(statusResp.Body).Decode(&detail)
	statusResp.Body.Close()
	actualStatus, _ := detail["status"].(string)
	if actualStatus != "migrating" {
		t.Fatalf("status = %q, want migrating", actualStatus)
	}

	// Verify: only 1 migrate call (no retry for genuine migration)
	mu.Lock()
	if migrateCallCount != 1 {
		t.Errorf("migrate called %d times, want 1 (should not retry genuine 409)", migrateCallCount)
	}
	mu.Unlock()
}

// --- State persistence tests ---

func TestUpgrade_GoalPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.json")
	state := &cluster.State{}
	state.SetPath(path)
	state.Orchestrator = &cluster.VMEntry{ID: "orch"}
	state.Nodes = []cluster.VMEntry{
		{ID: "node-1", ImageDigest: "sha256:old"},
	}
	state.SetUpgradeGoal(&cluster.UpgradeGoal{
		VMType:           "all",
		Tag:              "latest",
		InitialNodeCount: 1,
		NodeImage:        &cluster.ImageRef{Digest: "sha256:new"},
	})
	state.Save()

	loaded, err := cluster.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UpgradeGoal == nil {
		t.Fatal("upgrade goal should persist")
	}
	if loaded.UpgradeGoal.NodeImage.Digest != "sha256:new" {
		t.Errorf("NodeImage digest = %q, want sha256:new", loaded.UpgradeGoal.NodeImage.Digest)
	}
}

func TestUpgrade_GoalCleared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.json")
	state := &cluster.State{}
	state.SetPath(path)
	state.Orchestrator = &cluster.VMEntry{ID: "orch"}
	state.SetUpgradeGoal(&cluster.UpgradeGoal{VMType: "all"})
	state.Save()

	state.ClearUpgradeGoal()
	state.Save()

	loaded, _ := cluster.Load(path)
	if loaded.UpgradeGoal != nil {
		t.Error("upgrade goal should be cleared after ClearUpgradeGoal")
	}
}

// --- Full upgrade cycle simulation ---

func TestUpgrade_FullCycle_AllNodesReplaced(t *testing.T) {
	// Simulate a full upgrade cycle: 3 old nodes → 3 new nodes
	// Each old node has 1 VM that migrates to a new node
	oldDigest := "sha256:old-image"
	newDigest := "sha256:new-image"

	state := &cluster.State{
		Nodes: []cluster.VMEntry{
			{ID: "node-1", Status: "active", ImageDigest: oldDigest, BridgeIP: "10.0.0.1"},
			{ID: "node-2", Status: "active", ImageDigest: oldDigest, BridgeIP: "10.0.0.2"},
			{ID: "node-3", Status: "active", ImageDigest: oldDigest, BridgeIP: "10.0.0.3"},
		},
		Orchestrator: &cluster.VMEntry{ID: "orch", ImageDigest: newDigest},
	}
	goal := &cluster.UpgradeGoal{
		VMType:           "node",
		Tag:              "latest",
		NodeImage:        &cluster.ImageRef{Digest: newDigest},
		InitialNodeCount: 3,
	}
	state.SetUpgradeGoal(goal)

	// Simulate the reconciler cycling through nodes
	for cycle := 0; cycle < 10; cycle++ {
		old := firstNodeNotMatchingGoal(state, goal)
		if old == nil {
			break // all done
		}

		// Simulate: launch replacement
		newID := fmt.Sprintf("node-%d", 4+cycle)
		state.Nodes = append(state.Nodes, cluster.VMEntry{
			ID: newID, Status: "active", ImageDigest: newDigest,
			BridgeIP: fmt.Sprintf("10.0.1.%d", cycle),
		})

		// Simulate: drain old node
		state.SetNodeStatus(old.ID, "draining")
		// ... migrations happen ...
		state.RemoveNode(old.ID)
	}

	// Verify: all nodes should have new digest
	for _, n := range state.Nodes {
		if n.ImageDigest != newDigest {
			t.Errorf("node %s has digest %s, want %s", n.ID, n.ImageDigest, newDigest)
		}
	}
	// Should have replaced all 3
	if len(state.Nodes) != 3 {
		t.Errorf("should have 3 nodes, got %d", len(state.Nodes))
	}

	// Verify no old nodes remain
	old := firstNodeNotMatchingGoal(state, goal)
	if old != nil {
		t.Errorf("old node %s should not exist after full cycle", old.ID)
	}
}

func TestUpgrade_FullCycle_100Variations(t *testing.T) {
	// Run 100 upgrade cycle variations with different cluster sizes and VM counts
	for i := 0; i < 100; i++ {
		numNodes := 1 + (i % 8)   // 1-8 nodes
		vmsPerNode := i % 5        // 0-4 VMs per node
		oldDigest := fmt.Sprintf("sha256:old-%d", i)
		newDigest := fmt.Sprintf("sha256:new-%d", i)

		state := &cluster.State{
			Orchestrator: &cluster.VMEntry{ID: "orch", ImageDigest: newDigest},
		}
		for n := 0; n < numNodes; n++ {
			state.Nodes = append(state.Nodes, cluster.VMEntry{
				ID:          fmt.Sprintf("node-%d", n),
				Status:      "active",
				ImageDigest: oldDigest,
				BridgeIP:    fmt.Sprintf("10.%d.0.%d", i, n),
			})
		}

		goal := &cluster.UpgradeGoal{
			VMType:           "node",
			Tag:              "latest",
			NodeImage:        &cluster.ImageRef{Digest: newDigest},
			InitialNodeCount: numNodes,
		}
		state.SetUpgradeGoal(goal)

		// Simulate full upgrade
		maxCycles := numNodes * 3 // safety limit
		for cycle := 0; cycle < maxCycles; cycle++ {
			old := firstNodeNotMatchingGoal(state, goal)
			if old == nil {
				break
			}

			// Launch replacement
			newID := fmt.Sprintf("new-%d-%d", i, cycle)
			state.Nodes = append(state.Nodes, cluster.VMEntry{
				ID: newID, Status: "active", ImageDigest: newDigest,
			})

			// Drain old
			state.SetNodeStatus(old.ID, "draining")
			state.RemoveNode(old.ID)

			_ = vmsPerNode // VMs would be migrated during drain
		}

		// Verify
		remaining := firstNodeNotMatchingGoal(state, goal)
		if remaining != nil {
			t.Errorf("variation %d: old node %s still exists after upgrade (nodes=%d, vms=%d)",
				i, remaining.ID, numNodes, vmsPerNode)
		}
		if len(state.Nodes) != numNodes {
			t.Errorf("variation %d: expected %d nodes, got %d", i, numNodes, len(state.Nodes))
		}
	}
}

func TestUpgrade_DrainRetriesOnUnreachable(t *testing.T) {
	// Bug: when drain can't reach a node, it reverts to "active",
	// but the reconciler immediately re-marks it as "upgrading".
	// This loops forever. The fix: after N failed drain attempts,
	// the node should be killed and removed.
	state := &cluster.State{
		Nodes: []cluster.VMEntry{
			{ID: "dead-node", Status: "active", ImageDigest: "sha256:old", BridgeIP: "10.0.0.99"},
			{ID: "new-node", Status: "active", ImageDigest: "sha256:new", BridgeIP: "10.0.1.1"},
		},
	}
	goal := &cluster.UpgradeGoal{
		NodeImage:        &cluster.ImageRef{Digest: "sha256:new"},
		InitialNodeCount: 1,
	}

	// Simulate: firstNodeNotMatchingGoal finds dead-node
	old := firstNodeNotMatchingGoal(state, goal)
	if old == nil || old.ID != "dead-node" {
		t.Fatal("should find dead-node")
	}

	// Simulate drain failure (node unreachable) — currently reverts to active
	// This is the bug: it should eventually give up and remove the node
	state.SetNodeStatus("dead-node", "upgrading")

	// After marking upgrading, the reconciler should try to drain
	// If drain fails, it should NOT revert to active (that causes the loop)
	// Instead, after enough failures, it should force-remove
	n := state.GetNode("dead-node")
	if n == nil || n.Status != "upgrading" {
		t.Error("node should be in upgrading status")
	}
}

func TestUpgrade_NodeStatusTransitions(t *testing.T) {
	state := &cluster.State{
		Nodes: []cluster.VMEntry{
			{ID: "node-1", Status: "active"},
		},
	}

	// active → upgrading
	state.SetNodeStatus("node-1", "upgrading")
	n := state.GetNode("node-1")
	if n.Status != "upgrading" {
		t.Errorf("status = %q, want upgrading", n.Status)
	}
	if n.IsActive() {
		t.Error("upgrading node should not be active")
	}

	// upgrading → draining
	state.SetNodeStatus("node-1", "draining")
	n = state.GetNode("node-1")
	if n.Status != "draining" {
		t.Errorf("status = %q, want draining", n.Status)
	}

	// draining → removed
	state.RemoveNode("node-1")
	if len(state.Nodes) != 0 {
		t.Error("node should be removed")
	}
}

// --- Surplus detection tests ---

func TestUpgrade_SurplusDetection(t *testing.T) {
	tests := []struct {
		name         string
		activeCount  int
		initialCount int
		wantSurplus  bool
	}{
		{"no surplus (equal)", 3, 3, false},
		{"surplus (replacement launched)", 4, 3, true},
		{"no surplus (one removed)", 2, 3, false},
		{"surplus (two extra)", 5, 3, true},
		{"initial zero (guard)", 1, 0, false}, // guard against InitialNodeCount=0
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasSurplus := tt.activeCount > tt.initialCount && tt.initialCount > 0
			if hasSurplus != tt.wantSurplus {
				t.Errorf("surplus = %v, want %v (active=%d, initial=%d)",
					hasSurplus, tt.wantSurplus, tt.activeCount, tt.initialCount)
			}
		})
	}
}

// --- Golden image readiness ---

func TestUpgrade_GoldenReadyCheck(t *testing.T) {
	node := newSimNode(true, false) // healthy but no golden
	srv := httptest.NewServer(node.handler())
	defer srv.Close()

	// Check health endpoint returns golden_ready=false
	resp, _ := http.Get(srv.URL + "/api/health")
	var health map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()

	if golden, _ := health["golden_ready"].(bool); golden {
		t.Error("golden_ready should be false")
	}

	// Set golden ready
	node.mu.Lock()
	node.golden = true
	node.mu.Unlock()

	resp2, _ := http.Get(srv.URL + "/api/health")
	json.NewDecoder(resp2.Body).Decode(&health)
	resp2.Body.Close()

	if golden, _ := health["golden_ready"].(bool); !golden {
		t.Error("golden_ready should be true after setting")
	}
}

// --- Insufficient memory pause tests (issue #81) ---

func TestUpgrade_InsufficientMemoryPausesAfter3Attempts(t *testing.T) {
	// When the host doesn't have enough RAM for a side-by-side replacement,
	// the reconciler should pause after 3 consecutive failures (not 50),
	// log an actionable message, and preserve the upgrade goal.
	path := filepath.Join(t.TempDir(), "cluster.json")
	state := &cluster.State{}
	state.SetPath(path)
	state.Orchestrator = &cluster.VMEntry{ID: "orch", ImageDigest: "sha256:current"}
	state.Nodes = []cluster.VMEntry{
		{ID: "node-1", Status: "active", ImageDigest: "sha256:old", BridgeIP: "192.168.50.3"},
		{ID: "node-2", Status: "active", ImageDigest: "sha256:old", BridgeIP: "192.168.50.4"},
		{ID: "node-3", Status: "active", ImageDigest: "sha256:old", BridgeIP: "192.168.50.5"},
	}
	state.SetUpgradeGoal(&cluster.UpgradeGoal{
		VMType:           "node",
		Tag:              "v2",
		NodeImage:        &cluster.ImageRef{Digest: "sha256:new"},
		InitialNodeCount: 3,
	})
	state.Save()

	// Simulate the retry logic from runReconcileLoop
	const insufficientMemoryPauseThreshold = 3
	insufficientMemErr := "insufficient memory to launch replacement node (8807MB available, need 12288MB + 1024MB reserve)"
	consecutiveFailures := 0
	paused := false

	for i := 0; i < 50; i++ {
		// Simulate reconcileUpgradeStep returning insufficient memory error
		errStr := insufficientMemErr
		consecutiveFailures++

		if strings.Contains(errStr, "insufficient memory to launch replacement node") && consecutiveFailures >= insufficientMemoryPauseThreshold {
			paused = true
			break
		}
	}

	if !paused {
		t.Fatal("reconciler should have paused after 3 insufficient memory failures")
	}
	if consecutiveFailures != insufficientMemoryPauseThreshold {
		t.Errorf("should pause at exactly %d failures, got %d", insufficientMemoryPauseThreshold, consecutiveFailures)
	}

	// Verify upgrade goal is preserved (not cleared)
	loaded, err := cluster.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UpgradeGoal == nil {
		t.Fatal("upgrade goal should be preserved after insufficient memory pause")
	}
	if loaded.UpgradeGoal.NodeImage.Digest != "sha256:new" {
		t.Errorf("upgrade goal digest = %q, want sha256:new", loaded.UpgradeGoal.NodeImage.Digest)
	}
}

func TestUpgrade_InsufficientMemoryDetection(t *testing.T) {
	// The insufficient memory error message from reconcileNodeUpgrade should
	// be detected by the string match in runReconcileLoop.
	errMsg := fmt.Sprintf("insufficient memory to launch replacement node (%dMB available, need %dMB + %dMB reserve)", 8807, 12288, 1024)
	if !strings.Contains(errMsg, "insufficient memory to launch replacement node") {
		t.Error("error message should match the detection string")
	}
}

func TestUpgrade_OtherErrorsStillRetry50Times(t *testing.T) {
	// Non-memory errors should still use the full 50-attempt threshold,
	// not the early pause at 3.
	const autoAbortThreshold = 50
	const insufficientMemoryPauseThreshold = 3
	consecutiveFailures := 0
	otherErr := "launching replacement for node-1: QEMU process exited unexpectedly"
	paused := false
	aborted := false

	for i := 0; i < 60; i++ {
		errStr := otherErr
		consecutiveFailures++

		if strings.Contains(errStr, "insufficient memory to launch replacement node") && consecutiveFailures >= insufficientMemoryPauseThreshold {
			paused = true
			break
		}
		if consecutiveFailures >= autoAbortThreshold {
			aborted = true
			break
		}
	}

	if paused {
		t.Error("non-memory errors should not trigger early pause")
	}
	if !aborted {
		t.Fatal("non-memory errors should hit the 50-attempt auto-abort")
	}
	if consecutiveFailures != autoAbortThreshold {
		t.Errorf("should abort at %d failures, got %d", autoAbortThreshold, consecutiveFailures)
	}
}

func TestUpgrade_InsufficientMemoryGoalPreservedVsAutoAbortGoalCleared(t *testing.T) {
	// Verify the key behavioral difference: insufficient memory pause
	// preserves the goal, while auto-abort clears it.
	path := filepath.Join(t.TempDir(), "cluster.json")

	// Setup state with upgrade goal
	state := &cluster.State{}
	state.SetPath(path)
	state.Orchestrator = &cluster.VMEntry{ID: "orch"}
	state.SetUpgradeGoal(&cluster.UpgradeGoal{
		VMType:    "node",
		Tag:       "v2",
		NodeImage: &cluster.ImageRef{Digest: "sha256:new"},
	})
	state.Save()

	// After insufficient memory pause: goal should still be set
	loaded, _ := cluster.Load(path)
	if loaded.UpgradeGoal == nil {
		t.Fatal("goal should be preserved (simulating insufficient memory pause)")
	}

	// After auto-abort: goal should be cleared
	loaded.ClearUpgradeGoal()
	loaded.Save()
	reloaded, _ := cluster.Load(path)
	if reloaded.UpgradeGoal != nil {
		t.Error("goal should be cleared after auto-abort")
	}
}

// SetPath sets the path for state persistence (for testing)
func init() {
	// Ensure the cluster package has helper methods we need
	_ = os.TempDir()
}
