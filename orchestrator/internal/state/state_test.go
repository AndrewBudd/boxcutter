package state

import (
	"sort"
	"sync"
	"testing"
)

func TestNodeCRUD(t *testing.T) {
	s := New()

	// Empty initially
	if nodes := s.ListNodes(); len(nodes) != 0 {
		t.Fatalf("ListNodes = %d, want 0", len(nodes))
	}

	// Add nodes
	s.SetNode(&Node{ID: "node-1", TailscaleName: "n1", APIAddr: "1:8800", Status: "active"})
	s.SetNode(&Node{ID: "node-2", TailscaleName: "n2", APIAddr: "2:8800", Status: "active"})

	if nodes := s.ListNodes(); len(nodes) != 2 {
		t.Fatalf("ListNodes = %d, want 2", len(nodes))
	}

	// Get
	n := s.GetNode("node-1")
	if n == nil {
		t.Fatal("GetNode(node-1) = nil")
	}
	if n.TailscaleName != "n1" {
		t.Errorf("TailscaleName = %q, want n1", n.TailscaleName)
	}

	// Get returns a copy (mutations don't affect store)
	n.TailscaleName = "modified"
	n2 := s.GetNode("node-1")
	if n2.TailscaleName != "n1" {
		t.Error("GetNode returned a reference, not a copy")
	}

	// Not found
	if got := s.GetNode("nonexistent"); got != nil {
		t.Error("GetNode(nonexistent) should return nil")
	}

	// Update (upsert)
	s.SetNode(&Node{ID: "node-1", TailscaleName: "n1-updated", APIAddr: "1:8800", Status: "active"})
	if got := s.GetNode("node-1"); got.TailscaleName != "n1-updated" {
		t.Errorf("After upsert, TailscaleName = %q, want n1-updated", got.TailscaleName)
	}

	// Delete
	s.DeleteNode("node-1")
	if got := s.GetNode("node-1"); got != nil {
		t.Error("After delete, GetNode should return nil")
	}
	if nodes := s.ListNodes(); len(nodes) != 1 {
		t.Fatalf("After delete, ListNodes = %d, want 1", len(nodes))
	}
}

func TestNodeStatus(t *testing.T) {
	s := New()
	s.SetNode(&Node{ID: "node-1", Status: "active"})

	s.SetNodeStatus("node-1", "down")
	if n := s.GetNode("node-1"); n.Status != "down" {
		t.Errorf("Status = %q, want down", n.Status)
	}

	// No-op for nonexistent node
	s.SetNodeStatus("nonexistent", "active")
}

func TestActiveNodes(t *testing.T) {
	s := New()
	s.SetNode(&Node{ID: "node-1", Status: "active"})
	s.SetNode(&Node{ID: "node-2", Status: "down"})
	s.SetNode(&Node{ID: "node-3", Status: "active"})

	active := s.ActiveNodes()
	if len(active) != 2 {
		t.Fatalf("ActiveNodes = %d, want 2", len(active))
	}
}

func TestVMCRUD(t *testing.T) {
	s := New()

	if vms := s.ListVMs(); len(vms) != 0 {
		t.Fatalf("ListVMs = %d, want 0", len(vms))
	}

	s.SetVM(&VM{Name: "vm-a", NodeID: "node-1", Status: "running"})
	s.SetVM(&VM{Name: "vm-b", NodeID: "node-1", Status: "running"})

	if vms := s.ListVMs(); len(vms) != 2 {
		t.Fatalf("ListVMs = %d, want 2", len(vms))
	}

	v := s.GetVM("vm-a")
	if v == nil {
		t.Fatal("GetVM(vm-a) = nil")
	}
	if v.NodeID != "node-1" {
		t.Errorf("NodeID = %q, want node-1", v.NodeID)
	}

	// Returns copy
	v.Status = "modified"
	if got := s.GetVM("vm-a"); got.Status != "running" {
		t.Error("GetVM returned a reference, not a copy")
	}

	// Not found
	if got := s.GetVM("nonexistent"); got != nil {
		t.Error("GetVM(nonexistent) should return nil")
	}

	// Update status
	s.UpdateVMStatus("vm-a", "stopped")
	if got := s.GetVM("vm-a"); got.Status != "stopped" {
		t.Errorf("Status = %q, want stopped", got.Status)
	}

	// Delete
	s.DeleteVM("vm-a")
	if got := s.GetVM("vm-a"); got != nil {
		t.Error("After delete, GetVM should return nil")
	}
}

func TestSyncNodeVMs(t *testing.T) {
	s := New()

	// Initial sync
	s.SyncNodeVMs("node-1", []VM{
		{Name: "vm-a", NodeID: "node-1", Status: "running"},
		{Name: "vm-b", NodeID: "node-1", Status: "running"},
	})

	if vms := s.ListVMs(); len(vms) != 2 {
		t.Fatalf("ListVMs = %d, want 2", len(vms))
	}

	// Re-sync: vm-a removed, vm-c added
	s.SyncNodeVMs("node-1", []VM{
		{Name: "vm-b", NodeID: "node-1", Status: "running"},
		{Name: "vm-c", NodeID: "node-1", Status: "running"},
	})

	vms := s.ListVMs()
	if len(vms) != 2 {
		t.Fatalf("After re-sync, ListVMs = %d, want 2", len(vms))
	}

	names := map[string]bool{}
	for _, v := range vms {
		names[v.Name] = true
	}
	if names["vm-a"] {
		t.Error("vm-a should have been removed by sync")
	}
	if !names["vm-c"] {
		t.Error("vm-c should have been added by sync")
	}
}

func TestSyncNodeVMs_MultiNode(t *testing.T) {
	s := New()

	s.SyncNodeVMs("node-1", []VM{
		{Name: "vm-a", NodeID: "node-1", Status: "running"},
	})
	s.SyncNodeVMs("node-2", []VM{
		{Name: "vm-b", NodeID: "node-2", Status: "running"},
	})

	if vms := s.ListVMs(); len(vms) != 2 {
		t.Fatalf("ListVMs = %d, want 2", len(vms))
	}

	// Syncing node-1 should not affect node-2's VMs
	s.SyncNodeVMs("node-1", []VM{})
	vms := s.ListVMs()
	if len(vms) != 1 {
		t.Fatalf("After clearing node-1, ListVMs = %d, want 1", len(vms))
	}
	if vms[0].Name != "vm-b" {
		t.Errorf("Remaining VM = %q, want vm-b", vms[0].Name)
	}
}

func TestGoldenImages(t *testing.T) {
	s := New()

	if images := s.ListGoldenImages(); len(images) != 0 {
		t.Fatalf("ListGoldenImages = %d, want 0", len(images))
	}

	s.SetGoldenImages("node-1", []string{"v1.0", "v1.1"})
	s.SetGoldenImages("node-2", []string{"v1.1", "v1.2"})

	images := s.ListGoldenImages()
	if len(images) != 4 {
		t.Fatalf("ListGoldenImages = %d, want 4", len(images))
	}

	// Re-sync node-1 with fewer versions
	s.SetGoldenImages("node-1", []string{"v1.1"})

	images = s.ListGoldenImages()
	// Should have: v1.1 on node-1, v1.1 on node-2, v1.2 on node-2 = 3
	if len(images) != 3 {
		t.Fatalf("After re-sync, ListGoldenImages = %d, want 3", len(images))
	}

	// Verify v1.0 is gone (was only on node-1)
	for _, img := range images {
		if img.Version == "v1.0" {
			t.Error("v1.0 should have been removed")
		}
	}
}

func TestConcurrency(t *testing.T) {
	s := New()
	var wg sync.WaitGroup

	// Concurrent writes and reads should not panic
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			id := "node-" + string(rune('a'+i%26))
			s.SetNode(&Node{ID: id, Status: "active"})
			s.GetNode(id)
			s.ListNodes()
			s.ActiveNodes()
		}(i)
		go func(i int) {
			defer wg.Done()
			name := "vm-" + string(rune('a'+i%26))
			s.SetVM(&VM{Name: name, NodeID: "node-a", Status: "running"})
			s.GetVM(name)
			s.ListVMs()
		}(i)
		go func(i int) {
			defer wg.Done()
			s.SetGoldenImages("node-a", []string{"v1.0"})
			s.ListGoldenImages()
		}(i)
	}
	wg.Wait()
}

func TestListNodesReturnsCopies(t *testing.T) {
	s := New()
	s.SetNode(&Node{ID: "node-1", Status: "active", TailscaleName: "original"})

	nodes := s.ListNodes()
	nodes[0].TailscaleName = "mutated"

	// Store should be unaffected
	if n := s.GetNode("node-1"); n.TailscaleName != "original" {
		t.Error("ListNodes returned references instead of copies")
	}
}

func TestListVMsReturnsCopies(t *testing.T) {
	s := New()
	s.SetVM(&VM{Name: "vm-a", NodeID: "node-1", Status: "running"})

	vms := s.ListVMs()
	vms[0].Status = "mutated"

	if v := s.GetVM("vm-a"); v.Status != "running" {
		t.Error("ListVMs returned references instead of copies")
	}
}

func TestUpdateNodeLastSeen(t *testing.T) {
	s := New()
	s.SetNode(&Node{ID: "node-1", Status: "active"})

	s.UpdateNodeLastSeen("node-1")
	n := s.GetNode("node-1")
	if n.LastSeen == "" {
		t.Error("LastSeen should be set after UpdateNodeLastSeen")
	}

	// No-op for nonexistent
	s.UpdateNodeLastSeen("nonexistent")
}

func TestSyncNodeVMs_Empty(t *testing.T) {
	s := New()
	s.SetVM(&VM{Name: "vm-a", NodeID: "node-1", Status: "running"})

	s.SyncNodeVMs("node-1", []VM{})

	if vms := s.ListVMs(); len(vms) != 0 {
		t.Errorf("After empty sync, ListVMs = %d, want 0", len(vms))
	}
}

func TestGoldenImages_ClearAll(t *testing.T) {
	s := New()
	s.SetGoldenImages("node-1", []string{"v1.0", "v1.1"})

	// Clear by setting empty
	s.SetGoldenImages("node-1", []string{})

	if images := s.ListGoldenImages(); len(images) != 0 {
		t.Errorf("After clearing, ListGoldenImages = %d, want 0", len(images))
	}
}

func TestActiveNodes_NoneActive(t *testing.T) {
	s := New()
	s.SetNode(&Node{ID: "node-1", Status: "down"})
	s.SetNode(&Node{ID: "node-2", Status: "draining"})

	active := s.ActiveNodes()
	if len(active) != 0 {
		t.Errorf("ActiveNodes = %d, want 0", len(active))
	}
}

func TestDeleteVM_Nonexistent(t *testing.T) {
	s := New()
	// Should not panic
	s.DeleteVM("nonexistent")
}

func TestDeleteNode_Nonexistent(t *testing.T) {
	s := New()
	// Should not panic
	s.DeleteNode("nonexistent")
}

func TestSyncNodeVMs_StatusPreserved(t *testing.T) {
	s := New()
	s.SyncNodeVMs("node-1", []VM{
		{Name: "vm-a", NodeID: "node-1", Status: "stopped"},
		{Name: "vm-b", NodeID: "node-1", Status: "running"},
	})

	a := s.GetVM("vm-a")
	b := s.GetVM("vm-b")
	if a.Status != "stopped" {
		t.Errorf("vm-a status = %q, want stopped", a.Status)
	}
	if b.Status != "running" {
		t.Errorf("vm-b status = %q, want running", b.Status)
	}
}

func TestGoldenImages_SortedOrder(t *testing.T) {
	s := New()
	s.SetGoldenImages("node-1", []string{"v1.2", "v1.0", "v1.1"})

	images := s.ListGoldenImages()
	sort.Slice(images, func(i, j int) bool {
		return images[i].Version < images[j].Version
	})

	if len(images) != 3 {
		t.Fatalf("ListGoldenImages = %d, want 3", len(images))
	}
	if images[0].Version != "v1.0" || images[1].Version != "v1.1" || images[2].Version != "v1.2" {
		t.Errorf("versions not as expected: %v", images)
	}
}
