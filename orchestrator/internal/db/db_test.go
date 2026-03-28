package db

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrate_Idempotent(t *testing.T) {
	d := openTestDB(t)
	// migrate() already ran in Open(). Run it again — should not error.
	if err := d.migrate(); err != nil {
		t.Errorf("Second migrate() failed: %v", err)
	}
}

func TestRegisterNode(t *testing.T) {
	d := openTestDB(t)
	n := &Node{
		ID:            "node-1",
		TailscaleName: "boxcutter-node-1",
		BridgeIP:      "192.168.50.3",
		APIAddr:       "192.168.50.3:8800",
		Status:        "active",
		RegisteredAt:  "2026-03-01T00:00:00Z",
	}
	if err := d.RegisterNode(n); err != nil {
		t.Fatal(err)
	}

	nodes, err := d.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("ListNodes returned %d nodes, want 1", len(nodes))
	}
	if nodes[0].ID != "node-1" {
		t.Errorf("node ID = %q, want node-1", nodes[0].ID)
	}
	if nodes[0].Status != "active" {
		t.Errorf("node status = %q, want active", nodes[0].Status)
	}
}

func TestRegisterNode_Upsert(t *testing.T) {
	d := openTestDB(t)
	n := &Node{
		ID: "node-1", TailscaleName: "boxcutter-node-1",
		BridgeIP: "192.168.50.3", APIAddr: "192.168.50.3:8800",
		Status: "active", RegisteredAt: "2026-03-01T00:00:00Z",
	}
	d.RegisterNode(n)

	// Re-register with updated IP
	n.BridgeIP = "192.168.50.99"
	n.TailscaleIP = "100.64.1.1"
	d.RegisterNode(n)

	nodes, _ := d.ListNodes()
	if len(nodes) != 1 {
		t.Fatalf("Upsert created duplicate: %d nodes", len(nodes))
	}
	if nodes[0].BridgeIP != "192.168.50.99" {
		t.Errorf("BridgeIP = %q, want 192.168.50.99 (should be updated)", nodes[0].BridgeIP)
	}
}

func TestSetNodeStatus(t *testing.T) {
	d := openTestDB(t)
	d.RegisterNode(&Node{
		ID: "node-1", TailscaleName: "n1", APIAddr: "1:8800",
		Status: "active", RegisteredAt: "2026-03-01T00:00:00Z",
	})

	if err := d.SetNodeStatus("node-1", "down"); err != nil {
		t.Fatal(err)
	}

	nodes, _ := d.ListNodes()
	if nodes[0].Status != "down" {
		t.Errorf("Status = %q, want down", nodes[0].Status)
	}

	// Recovery: down → active
	d.SetNodeStatus("node-1", "active")
	nodes, _ = d.ListNodes()
	if nodes[0].Status != "active" {
		t.Errorf("Status = %q, want active after recovery", nodes[0].Status)
	}
}

func TestSyncNodeVMs(t *testing.T) {
	d := openTestDB(t)
	d.RegisterNode(&Node{
		ID: "node-1", TailscaleName: "n1", APIAddr: "1:8800",
		Status: "active", RegisteredAt: "2026-03-01T00:00:00Z",
	})

	// Initial sync
	vms := []VM{
		{Name: "vm-a", NodeID: "node-1", Status: "running"},
		{Name: "vm-b", NodeID: "node-1", Status: "running"},
	}
	if err := d.SyncNodeVMs("node-1", vms); err != nil {
		t.Fatal(err)
	}

	all, _ := d.ListVMs()
	if len(all) != 2 {
		t.Fatalf("ListVMs = %d, want 2", len(all))
	}

	// Sync again with different set — vm-a removed, vm-c added
	vms2 := []VM{
		{Name: "vm-b", NodeID: "node-1", Status: "running"},
		{Name: "vm-c", NodeID: "node-1", Status: "running"},
	}
	d.SyncNodeVMs("node-1", vms2)

	all, _ = d.ListVMs()
	if len(all) != 2 {
		t.Fatalf("ListVMs after re-sync = %d, want 2", len(all))
	}

	names := map[string]bool{}
	for _, v := range all {
		names[v.Name] = true
	}
	if names["vm-a"] {
		t.Error("vm-a should have been removed by sync")
	}
	if !names["vm-c"] {
		t.Error("vm-c should have been added by sync")
	}
}

func TestSyncNodeVMs_EmptyList(t *testing.T) {
	d := openTestDB(t)
	d.RegisterNode(&Node{
		ID: "node-1", TailscaleName: "n1", APIAddr: "1:8800",
		Status: "active", RegisteredAt: "2026-03-01T00:00:00Z",
	})

	// Add VMs then sync with empty
	d.SyncNodeVMs("node-1", []VM{{Name: "vm-a", NodeID: "node-1", Status: "running"}})
	d.SyncNodeVMs("node-1", []VM{})

	all, _ := d.ListVMs()
	if len(all) != 0 {
		t.Errorf("ListVMs after empty sync = %d, want 0", len(all))
	}
}

func TestUpdateNodeHeartbeat(t *testing.T) {
	d := openTestDB(t)
	d.RegisterNode(&Node{
		ID: "node-1", TailscaleName: "n1", APIAddr: "1:8800",
		Status: "active", RegisteredAt: "2026-03-01T00:00:00Z",
	})

	ts := "2026-03-28T16:00:00Z"
	if err := d.UpdateNodeHeartbeat("node-1", ts); err != nil {
		t.Fatal(err)
	}

	nodes, _ := d.ListNodes()
	if nodes[0].LastHeartbeat != ts {
		t.Errorf("LastHeartbeat = %q, want %q", nodes[0].LastHeartbeat, ts)
	}
}
