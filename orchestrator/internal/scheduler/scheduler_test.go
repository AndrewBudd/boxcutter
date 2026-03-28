package scheduler

import (
	"testing"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
)

func TestPickNode_MostFreeRAM(t *testing.T) {
	nodes := []*db.Node{
		{ID: "n1", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 8000},
		{ID: "n2", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 2000},
		{ID: "n3", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 6000},
	}
	n, err := PickNode(nodes, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "n2" {
		t.Errorf("PickNode = %s, want n2 (most free RAM)", n.ID)
	}
}

func TestPickNode_SkipsInactive(t *testing.T) {
	nodes := []*db.Node{
		{ID: "n1", Status: "down", RAMTotalMIB: 12288, RAMAllocatedMIB: 0},
		{ID: "n2", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 8000},
	}
	n, err := PickNode(nodes, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "n2" {
		t.Errorf("PickNode = %s, want n2 (only active node)", n.ID)
	}
}

func TestPickNode_AllDown(t *testing.T) {
	nodes := []*db.Node{
		{ID: "n1", Status: "down", RAMTotalMIB: 12288, RAMAllocatedMIB: 0},
		{ID: "n2", Status: "draining", RAMTotalMIB: 12288, RAMAllocatedMIB: 0},
	}
	_, err := PickNode(nodes, 2048)
	if err == nil {
		t.Error("PickNode should error when all nodes are down")
	}
}

func TestPickNode_NoNodes(t *testing.T) {
	_, err := PickNode(nil, 2048)
	if err == nil {
		t.Error("PickNode should error with nil nodes")
	}
}

func TestPickNode_EmptySlice(t *testing.T) {
	_, err := PickNode([]*db.Node{}, 2048)
	if err == nil {
		t.Error("PickNode should error with empty nodes")
	}
}

func TestPickNode_FallbackWhenNoNodeMeetsRAM(t *testing.T) {
	nodes := []*db.Node{
		{ID: "n1", Status: "active", RAMTotalMIB: 4096, RAMAllocatedMIB: 3000},
		{ID: "n2", Status: "active", RAMTotalMIB: 4096, RAMAllocatedMIB: 2000},
	}
	// Require 8192 MiB — neither can satisfy, but fallback picks most-free
	n, err := PickNode(nodes, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "n2" {
		t.Errorf("PickNode fallback = %s, want n2 (most free even though insufficient)", n.ID)
	}
}

func TestPickNode_SkipsDraining(t *testing.T) {
	nodes := []*db.Node{
		{ID: "n1", Status: "draining", RAMTotalMIB: 12288, RAMAllocatedMIB: 0},
		{ID: "n2", Status: "upgrading", RAMTotalMIB: 12288, RAMAllocatedMIB: 0},
		{ID: "n3", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 10000},
	}
	n, err := PickNode(nodes, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "n3" {
		t.Errorf("PickNode = %s, want n3 (only active)", n.ID)
	}
}
