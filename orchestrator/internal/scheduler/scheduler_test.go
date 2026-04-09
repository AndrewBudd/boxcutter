package scheduler

import (
	"testing"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
)

func TestPickNode_SpreadsToLeastLoaded(t *testing.T) {
	// n1: 65% utilization (4000/12288 free), n2: 16% utilization (10288/12288 free)
	// Both can fit VM, both below 80% — pick n2 (most free)
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
		t.Errorf("PickNode = %s, want n2 (least loaded)", n.ID)
	}
}

func TestPickNode_PrefersCoolOverHot(t *testing.T) {
	// n1: 90% util, 1288 free (hot, but has capacity)
	// n2: 50% util, 6144 free (cool)
	// n3: 95% util, 614 free (hot, insufficient)
	nodes := []*db.Node{
		{ID: "n1", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 11000},
		{ID: "n2", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 6144},
		{ID: "n3", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 11674},
	}
	n, err := PickNode(nodes, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "n2" {
		t.Errorf("PickNode = %s, want n2 (cool node preferred over hot n1)", n.ID)
	}
}

func TestPickNode_UsesHotNodeWhenNoCool(t *testing.T) {
	// Both nodes above 80% — pick the one with more free RAM
	nodes := []*db.Node{
		{ID: "n1", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 10500}, // 85% used, 1788 free
		{ID: "n2", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 10000}, // 81% used, 2288 free
	}
	n, err := PickNode(nodes, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "n2" {
		t.Errorf("PickNode = %s, want n2 (more free RAM among hot nodes)", n.ID)
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

func TestPickNode_PrefersActualAvailableRAM(t *testing.T) {
	// n1 has more declared free (10288) but less actual available (3000)
	// n2 has less declared free (4288) but more actual available (7500)
	nodes := []*db.Node{
		{ID: "n1", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 2000, RAMAvailableMIB: 3000},
		{ID: "n2", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 8000, RAMAvailableMIB: 7500},
	}
	n, err := PickNode(nodes, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "n2" {
		t.Errorf("PickNode = %s, want n2 (more actual available RAM)", n.ID)
	}
}

func TestPickNode_FallsBackToDeclaredWhenNoAvailable(t *testing.T) {
	// RAMAvailableMIB is 0 (not reported), should use declared
	nodes := []*db.Node{
		{ID: "n1", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 8000},
		{ID: "n2", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 2000},
	}
	n, err := PickNode(nodes, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "n2" {
		t.Errorf("PickNode = %s, want n2 (most declared free RAM)", n.ID)
	}
}

func TestPickNode_HeadroomThreshold(t *testing.T) {
	// n1: exactly at 80% (2457/12288 free) — should be considered "hot"
	// n2: at 79% (2580/12288 free) — should be considered "cool"
	nodes := []*db.Node{
		{ID: "n1", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 9831}, // 80% used
		{ID: "n2", Status: "active", RAMTotalMIB: 12288, RAMAllocatedMIB: 9708}, // 79% used
	}
	n, err := PickNode(nodes, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "n2" {
		t.Errorf("PickNode = %s, want n2 (below headroom threshold)", n.ID)
	}
}

func TestPickNode_ThreeNodeSpread(t *testing.T) {
	// Simulates the OOM scenario from the issue: 3 nodes, ensure VMs spread
	// n1: heavy load (90%), n2: empty, n3: moderate (40%)
	// Should pick n2 (most free, below headroom)
	nodes := []*db.Node{
		{ID: "n1", Status: "active", RAMTotalMIB: 12000, RAMAllocatedMIB: 10800}, // 90%
		{ID: "n2", Status: "active", RAMTotalMIB: 12000, RAMAllocatedMIB: 0},     // 0%
		{ID: "n3", Status: "active", RAMTotalMIB: 12000, RAMAllocatedMIB: 4800},  // 40%
	}
	n, err := PickNode(nodes, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "n2" {
		t.Errorf("PickNode = %s, want n2 (empty node for spread)", n.ID)
	}
}

func TestUtilizationPercent(t *testing.T) {
	tests := []struct {
		name string
		node *db.Node
		want int
	}{
		{"empty node", &db.Node{RAMTotalMIB: 12288, RAMAllocatedMIB: 0}, 0},
		{"half loaded", &db.Node{RAMTotalMIB: 12288, RAMAllocatedMIB: 6144}, 50},
		{"fully loaded", &db.Node{RAMTotalMIB: 12288, RAMAllocatedMIB: 12288}, 100},
		{"zero total", &db.Node{RAMTotalMIB: 0}, 100},
		{"with available", &db.Node{RAMTotalMIB: 12288, RAMAvailableMIB: 3072}, 75},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := utilizationPercent(tt.node)
			if got != tt.want {
				t.Errorf("utilizationPercent() = %d, want %d", got, tt.want)
			}
		})
	}
}
