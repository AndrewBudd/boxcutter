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
