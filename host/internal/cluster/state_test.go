package cluster

import (
	"path/filepath"
	"testing"
)

func TestVMEntry_IsActive(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"", true},
		{"active", true},
		{"down", false},
		{"draining", false},
		{"upgrading", false},
		{"retired", false},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			e := VMEntry{Status: tt.status}
			if got := e.IsActive(); got != tt.want {
				t.Errorf("IsActive(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestVMEntry_MatchesImage_DigestPriority(t *testing.T) {
	tests := []struct {
		name      string
		entry     VMEntry
		ref       *ImageRef
		wantMatch bool
	}{
		{
			"matching digest",
			VMEntry{ImageDigest: "sha256:abc"},
			&ImageRef{Digest: "sha256:abc"},
			true,
		},
		{
			"different digest",
			VMEntry{ImageDigest: "sha256:abc"},
			&ImageRef{Digest: "sha256:def"},
			false,
		},
		{
			"matching digest ignores version",
			VMEntry{ImageDigest: "sha256:abc", ImageVersion: "v1"},
			&ImageRef{Digest: "sha256:abc", Version: "v2"},
			true,
		},
		{
			"no digest, matching version",
			VMEntry{ImageVersion: "v1"},
			&ImageRef{Version: "v1"},
			true,
		},
		{
			"no digest, different version",
			VMEntry{ImageVersion: "v1"},
			&ImageRef{Version: "v2"},
			false,
		},
		{
			"both empty",
			VMEntry{},
			&ImageRef{},
			false,
		},
		{
			"nil ref",
			VMEntry{ImageDigest: "sha256:abc"},
			nil,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.entry.MatchesImage(tt.ref); got != tt.wantMatch {
				t.Errorf("MatchesImage = %v, want %v", got, tt.wantMatch)
			}
		})
	}
}

func TestState_SaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.json")

	s := &State{path: path}
	s.Orchestrator = &VMEntry{ID: "orch", BridgeIP: "192.168.50.2"}
	s.Nodes = []VMEntry{
		{ID: "node-1", BridgeIP: "192.168.50.3"},
		{ID: "node-2", BridgeIP: "192.168.50.4"},
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Orchestrator == nil || loaded.Orchestrator.BridgeIP != "192.168.50.2" {
		t.Error("Orchestrator not loaded correctly")
	}
	if len(loaded.Nodes) != 2 {
		t.Errorf("Loaded %d nodes, want 2", len(loaded.Nodes))
	}
}

func TestState_SetNodeStatus(t *testing.T) {
	s := &State{
		Nodes: []VMEntry{
			{ID: "node-1", Status: "active"},
			{ID: "node-2", Status: "active"},
		},
	}

	s.SetNodeStatus("node-1", "draining")
	for _, n := range s.Nodes {
		if n.ID == "node-1" && n.Status != "draining" {
			t.Errorf("node-1 status = %q, want draining", n.Status)
		}
	}
}

func TestVMEntry_MatchesImage_StaleLatestTag(t *testing.T) {
	// Regression test: when the OCI 'latest' tag isn't updated during push,
	// the pulled image has the same digest as existing nodes, causing the
	// reconciler to skip all nodes (no cycling). This test verifies that
	// different digests correctly return false.
	oldDigest := "sha256:b35a822d13eed968ab86c50d081ab8aafc5274396b08108909e0385344d28e3c"
	newDigest := "sha256:05915f039c82f09c39f20de5f13a15ead54d3692622126ade783d0436e8e39c8"

	node := VMEntry{
		ID:          "boxcutter-node-15",
		ImageDigest: oldDigest,
	}
	newImage := &ImageRef{
		Digest:  newDigest,
		Version: "v0.12.1",
	}

	if node.MatchesImage(newImage) {
		t.Error("Node with old digest should NOT match new image — rolling upgrade must cycle this node")
	}
}

func TestVMEntry_MatchesImage_SameDigestSkips(t *testing.T) {
	// When a node already has the target image, the reconciler should skip it.
	digest := "sha256:05915f039c82f09c39f20de5f13a15ead54d3692622126ade783d0436e8e39c8"
	node := VMEntry{ID: "boxcutter-node-25", ImageDigest: digest}
	newImage := &ImageRef{Digest: digest}

	if !node.MatchesImage(newImage) {
		t.Error("Node with matching digest should be skipped (already upgraded)")
	}
}

func TestUpgradeGoal_NodeNeedsCycling(t *testing.T) {
	// Simulate the reconciler's perspective: 3 nodes with old digest,
	// goal has new digest. All 3 should need cycling.
	goal := &UpgradeGoal{
		VMType: "all",
		Tag:    "latest",
		NodeImage: &ImageRef{
			Digest:  "sha256:new-image-digest",
			Version: "v0.12.1",
		},
		InitialNodeCount: 3,
	}

	state := &State{
		Nodes: []VMEntry{
			{ID: "node-1", Status: "active", ImageDigest: "sha256:old-digest"},
			{ID: "node-2", Status: "active", ImageDigest: "sha256:old-digest"},
			{ID: "node-3", Status: "active", ImageDigest: "sha256:old-digest"},
		},
	}

	// All nodes should be flagged as needing upgrade
	for _, n := range state.Nodes {
		if n.MatchesImage(goal.NodeImage) {
			t.Errorf("Node %s with old digest should NOT match goal", n.ID)
		}
	}

	// After one node is replaced
	state.Nodes = append(state.Nodes, VMEntry{
		ID: "node-4", Status: "active", ImageDigest: "sha256:new-image-digest",
	})

	// The new node should match
	if !state.Nodes[3].MatchesImage(goal.NodeImage) {
		t.Error("Replacement node should match goal image")
	}

	// Old nodes still shouldn't
	if state.Nodes[0].MatchesImage(goal.NodeImage) {
		t.Error("Old node should still not match")
	}
}

func TestState_NextNodeNum(t *testing.T) {
	s := &State{
		Nodes: []VMEntry{
			{ID: "boxcutter-node-3"},
			{ID: "boxcutter-node-7"},
			{ID: "boxcutter-node-1"},
		},
	}
	got := s.NextNodeNum()
	if got != 8 {
		t.Errorf("NextNodeNum = %d, want 8 (max existing + 1)", got)
	}
}
