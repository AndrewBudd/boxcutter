package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/state"
)

// newTestHandler creates a handler with a real DB (for SSH keys/golden config)
// and an in-memory state store, without starting background goroutines.
func newTestHandler(t *testing.T) (*Handler, *state.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	store := state.New()
	h := &Handler{db: database, state: store}
	return h, store
}

func TestHealthzEndpoint(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "ok\n" {
		t.Errorf("body = %q, want ok", body)
	}
}

func TestNodeRegister(t *testing.T) {
	h, store := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"id":"node-1","tailscale_name":"n1","bridge_ip":"192.168.50.3","api_addr":"192.168.50.3:8800"}`
	req := httptest.NewRequest("POST", "/api/nodes/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}

	n := store.GetNode("node-1")
	if n == nil {
		t.Fatal("node not found in state store")
	}
	if n.Status != "active" {
		t.Errorf("status = %q, want active", n.Status)
	}
}

func TestNodeRegister_MissingID(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"tailscale_name":"n1"}`
	req := httptest.NewRequest("POST", "/api/nodes/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestNodeList(t *testing.T) {
	h, store := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	store.SetNode(&state.Node{ID: "node-1", TailscaleName: "n1", Status: "active"})
	store.SetNode(&state.Node{ID: "node-2", TailscaleName: "n2", Status: "down"})

	req := httptest.NewRequest("GET", "/api/nodes", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var nodes []*state.Node
	json.NewDecoder(w.Body).Decode(&nodes)
	if len(nodes) != 2 {
		t.Errorf("returned %d nodes, want 2", len(nodes))
	}
}

func TestNodeGet(t *testing.T) {
	h, store := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	store.SetNode(&state.Node{ID: "node-1", TailscaleName: "n1", Status: "active"})

	req := httptest.NewRequest("GET", "/api/nodes/node-1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestNodeGet_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/nodes/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestVMList_Empty(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/vms", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestVMGet_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/vms/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestVMDestroy_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("DELETE", "/api/vms/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestVMStop_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("POST", "/api/vms/nonexistent/stop", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestVMStart_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("POST", "/api/vms/nonexistent/start", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestVMCreate_NoActiveNodes(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"name":"test-vm","vcpu":2,"ram_mib":2048}`
	req := httptest.NewRequest("POST", "/api/vms", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestVMCreate_ExceedsMaxSize(t *testing.T) {
	h, _ := newTestHandler(t)
	h.MaxVMSizeMB = 4096
	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"name":"big-vm","vcpu":2,"ram_mib":8192}`
	req := httptest.NewRequest("POST", "/api/vms", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestVMCreate_Duplicate(t *testing.T) {
	h, store := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	store.SetVM(&state.VM{Name: "existing-vm", NodeID: "node-1", Status: "running"})

	body := `{"name":"existing-vm","vcpu":2,"ram_mib":2048}`
	req := httptest.NewRequest("POST", "/api/vms", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestGoldenHead(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	// Get initial (empty)
	req := httptest.NewRequest("GET", "/api/golden/head", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)
	if result["version"] != "" {
		t.Errorf("initial version = %q, want empty", result["version"])
	}

	// Set golden head
	body := `{"version":"v1.2.3"}`
	req = httptest.NewRequest("POST", "/api/golden/head", strings.NewReader(body))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("set status = %d, want 200", w.Code)
	}

	// Read it back
	req = httptest.NewRequest("GET", "/api/golden/head", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	json.NewDecoder(w.Body).Decode(&result)
	if result["version"] != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", result["version"])
	}
}

func TestGoldenSetHead_MissingVersion(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"version":""}`
	req := httptest.NewRequest("POST", "/api/golden/head", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestGoldenList(t *testing.T) {
	h, store := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	store.SetNode(&state.Node{ID: "node-1", TailscaleName: "n1", Status: "active"})
	store.SetGoldenImages("node-1", []string{"v1.0", "v1.1"})

	req := httptest.NewRequest("GET", "/api/golden", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var entries []goldenListEntry
	json.NewDecoder(w.Body).Decode(&entries)
	if len(entries) != 2 {
		t.Errorf("golden entries = %d, want 2", len(entries))
	}
}

func TestSSHKeysCRUD(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	// Add keys
	body := `{"github_user":"alice","keys":["ssh-ed25519 AAAA alice@dev"]}`
	req := httptest.NewRequest("POST", "/api/keys/add", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("add status = %d, want 200", w.Code)
	}

	// List keys
	req = httptest.NewRequest("GET", "/api/keys", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("list status = %d, want 200", w.Code)
	}

	var keys []*db.SSHKey
	json.NewDecoder(w.Body).Decode(&keys)
	if len(keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(keys))
	}
	if keys[0].GitHubUser != "alice" {
		t.Errorf("github_user = %q, want alice", keys[0].GitHubUser)
	}

	// Delete keys
	req = httptest.NewRequest("DELETE", "/api/keys/alice", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", w.Code)
	}

	// Verify empty
	req = httptest.NewRequest("GET", "/api/keys", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	json.NewDecoder(w.Body).Decode(&keys)
	if len(keys) != 0 {
		t.Errorf("after delete, keys = %d, want 0", len(keys))
	}
}

func TestHealth(t *testing.T) {
	h, store := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	store.SetNode(&state.Node{ID: "node-1", Status: "active"})
	store.SetNode(&state.Node{ID: "node-2", Status: "down"})
	store.SetVM(&state.VM{Name: "vm-a", NodeID: "node-1", Status: "running"})

	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var result map[string]interface{}
	json.NewDecoder(w.Body).Decode(&result)

	if nodesTotal := int(result["nodes_total"].(float64)); nodesTotal != 2 {
		t.Errorf("nodes_total = %d, want 2", nodesTotal)
	}
	if nodesActive := int(result["nodes_active"].(float64)); nodesActive != 1 {
		t.Errorf("nodes_active = %d, want 1", nodesActive)
	}
	if vmsTotal := int(result["vms_total"].(float64)); vmsTotal != 1 {
		t.Errorf("vms_total = %d, want 1", vmsTotal)
	}
}

func TestVMLocation_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/vms/nonexistent/location", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestVMLocation(t *testing.T) {
	h, store := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	store.SetNode(&state.Node{ID: "node-1", APIAddr: "192.168.50.3:8800", Status: "active"})
	store.SetVM(&state.VM{Name: "vm-a", NodeID: "node-1", Status: "running"})

	req := httptest.NewRequest("GET", "/api/vms/vm-a/location", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)
	if result["node_id"] != "node-1" {
		t.Errorf("node_id = %q, want node-1", result["node_id"])
	}
	if result["node_addr"] != "192.168.50.3:8800" {
		t.Errorf("node_addr = %q, want 192.168.50.3:8800", result["node_addr"])
	}
}

func TestTapegunMessage_VMNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"body":"hello","from":"admin"}`
	req := httptest.NewRequest("POST", "/api/tapegun/message/nonexistent", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestNodeHeartbeat(t *testing.T) {
	h, store := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	store.SetNode(&state.Node{ID: "node-1", Status: "active"})

	req := httptest.NewRequest("POST", "/api/nodes/node-1/heartbeat", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	n := store.GetNode("node-1")
	if n.LastSeen == "" {
		t.Error("LastSeen should be updated after heartbeat")
	}
}

func TestStatelessRestart(t *testing.T) {
	// Simulate what happens when the orchestrator restarts:
	// 1. New state store starts empty
	// 2. Nodes are "discovered" (simulated here by adding to store)
	// 3. VMs are synced from discovered nodes
	// 4. All state is available immediately

	store := state.New()

	// Simulate node discovery
	store.SetNode(&state.Node{
		ID: "node-1", TailscaleName: "n1", APIAddr: "192.168.50.3:8800",
		Status: "active", DiscoveredAt: "2026-01-01T00:00:00Z",
	})
	store.SetNode(&state.Node{
		ID: "node-2", TailscaleName: "n2", APIAddr: "192.168.50.4:8800",
		Status: "active", DiscoveredAt: "2026-01-01T00:00:00Z",
	})

	// Simulate VM sync from nodes
	store.SyncNodeVMs("node-1", []state.VM{
		{Name: "vm-a", NodeID: "node-1", Status: "running"},
		{Name: "vm-b", NodeID: "node-1", Status: "stopped"},
	})
	store.SyncNodeVMs("node-2", []state.VM{
		{Name: "vm-c", NodeID: "node-2", Status: "running"},
	})

	// Simulate golden image sync
	store.SetGoldenImages("node-1", []string{"v1.0", "v1.1"})
	store.SetGoldenImages("node-2", []string{"v1.1"})

	// Verify everything is accessible
	if nodes := store.ListNodes(); len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	if vms := store.ListVMs(); len(vms) != 3 {
		t.Fatalf("vms = %d, want 3", len(vms))
	}
	if images := store.ListGoldenImages(); len(images) != 3 {
		t.Fatalf("golden images = %d, want 3", len(images))
	}

	// Verify VM -> node lookup works
	vm := store.GetVM("vm-a")
	if vm == nil || vm.NodeID != "node-1" {
		t.Error("VM -> node lookup failed")
	}

	// Simulate second restart: new store, re-discover
	store2 := state.New()
	if vms := store2.ListVMs(); len(vms) != 0 {
		t.Fatal("new store should start empty")
	}

	// Re-discover (same data as before)
	store2.SetNode(&state.Node{ID: "node-1", Status: "active"})
	store2.SyncNodeVMs("node-1", []state.VM{
		{Name: "vm-a", NodeID: "node-1", Status: "running"},
		{Name: "vm-b", NodeID: "node-1", Status: "stopped"},
	})

	// State is fully rebuilt
	if vms := store2.ListVMs(); len(vms) != 2 {
		t.Fatalf("after re-discover, vms = %d, want 2", len(vms))
	}
}

func TestMigrationEndpointsRemoved(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	// Migration endpoints should no longer exist
	endpoints := []string{
		"/api/migrate",
		"/api/prepare-migrate",
		"/api/shutdown",
	}

	for _, ep := range endpoints {
		req := httptest.NewRequest("POST", ep, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		// 405 (method not allowed) or 404 means the route doesn't exist
		if w.Code == 200 || w.Code == 201 {
			t.Errorf("POST %s returned %d, expected migration endpoint to be removed", ep, w.Code)
		}
	}
}
