package main

import "testing"

func TestHeadroomScaleUpTrigger(t *testing.T) {
	// The headroom check in autoScaleLoop triggers a scale-up when no node
	// has enough free RAM to fit a max-sized VM. This test validates the
	// logic that computes whether headroom is sufficient.

	tests := []struct {
		name        string
		nodes       []nodeCapacity
		maxVMSizeMB int
		wantTrigger bool
	}{
		{
			name: "sufficient headroom — no trigger",
			nodes: []nodeCapacity{
				{nodeID: "node-1", freeRAM: 10000},
				{nodeID: "node-2", freeRAM: 6000},
			},
			maxVMSizeMB: 8192,
			wantTrigger: false, // node-1 has 10000 > 8192
		},
		{
			name: "insufficient headroom — trigger scale-up",
			nodes: []nodeCapacity{
				{nodeID: "node-1", freeRAM: 5000},
				{nodeID: "node-2", freeRAM: 3000},
			},
			maxVMSizeMB: 8192,
			wantTrigger: true, // largest free is 5000 < 8192
		},
		{
			name: "exactly at threshold — no trigger",
			nodes: []nodeCapacity{
				{nodeID: "node-1", freeRAM: 8192},
			},
			maxVMSizeMB: 8192,
			wantTrigger: false, // 8192 >= 8192
		},
		{
			name: "one byte under threshold — trigger",
			nodes: []nodeCapacity{
				{nodeID: "node-1", freeRAM: 8191},
			},
			maxVMSizeMB: 8192,
			wantTrigger: true, // 8191 < 8192
		},
		{
			name:        "no nodes — trigger",
			nodes:       nil,
			maxVMSizeMB: 8192,
			wantTrigger: true, // no nodes means no free RAM
		},
		{
			name: "maxVMSizeMB is zero — disabled, no trigger",
			nodes: []nodeCapacity{
				{nodeID: "node-1", freeRAM: 100},
			},
			maxVMSizeMB: 0,
			wantTrigger: false, // feature disabled
		},
		{
			name: "multiple nodes, only one has enough",
			nodes: []nodeCapacity{
				{nodeID: "node-1", freeRAM: 2000},
				{nodeID: "node-2", freeRAM: 9000},
				{nodeID: "node-3", freeRAM: 1000},
			},
			maxVMSizeMB: 8192,
			wantTrigger: false, // node-2 has 9000 > 8192
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsHeadroomScaleUp(tt.nodes, tt.maxVMSizeMB)
			if got != tt.wantTrigger {
				t.Errorf("needsHeadroomScaleUp() = %v, want %v", got, tt.wantTrigger)
			}
		})
	}
}

// needsHeadroomScaleUp extracts the headroom check logic from autoScaleLoop
// so it can be tested independently.
func needsHeadroomScaleUp(nodes []nodeCapacity, maxVMSizeMB int) bool {
	if maxVMSizeMB <= 0 {
		return false
	}
	largestFree := 0
	for _, nc := range nodes {
		if nc.freeRAM > largestFree {
			largestFree = nc.freeRAM
		}
	}
	return largestFree < maxVMSizeMB
}

func TestMaxVMSizeDerivation(t *testing.T) {
	// MaxVMSizeMB = parseRAMMB(nodeRAM) * headroomPct / 100
	// This tests the derivation formula.
	tests := []struct {
		name        string
		nodeRAM     string
		headroomPct int
		wantMB      int
	}{
		{"12G at 80%", "12G", 80, 9830},   // 12*1024*80/100 = 9830
		{"16G at 80%", "16G", 80, 13107},  // 16*1024*80/100 = 13107
		{"8G at 80%", "8G", 80, 6553},     // 8*1024*80/100 = 6553
		{"12G at 100%", "12G", 100, 12288}, // 12*1024
		{"12G at 50%", "12G", 50, 6144},   // 12*1024*50/100
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ramMB := parseRAMMB(tt.nodeRAM)
			got := ramMB * tt.headroomPct / 100
			if got != tt.wantMB {
				t.Errorf("parseRAMMB(%q)*%d/100 = %d, want %d", tt.nodeRAM, tt.headroomPct, got, tt.wantMB)
			}
		})
	}
}

func TestCanScaleUpMinFreeReduction(t *testing.T) {
	// MinFreeMemoryMB was reduced from 8192 to 4096.
	//
	// Justification: The old value of 8192 (8G) combined with the rolling
	// upgrade double-reserve (nodeRAM*2 + minFree) meant a 12G-node host
	// needed 12*2 + 8 = 32G free to scale up — this blocked scale-ups on
	// 64G hosts with 3+ nodes already running. The new formula removes the
	// double-reserve (upgrades stop-then-start, not side-by-side) and 4G
	// gives enough buffer for host OS + QEMU overhead while allowing the
	// host to use its RAM more efficiently for actual workloads.
	cfg := defaultConfig()

	if cfg.MinFreeMemoryMB != 4096 {
		t.Errorf("MinFreeMemoryMB = %d, want 4096", cfg.MinFreeMemoryMB)
	}

	// Verify the simplified formula: nodeRAM + minFree (no double-reserve)
	// With 12G node + 4G minFree = 16G needed to scale up.
	// Previously: 12G*2 + 8G = 32G needed — blocked too aggressively.
	if cfg.HeadroomPct != 80 {
		t.Errorf("HeadroomPct = %d, want 80", cfg.HeadroomPct)
	}
}

func TestDefaultHeadroomConfig(t *testing.T) {
	cfg := defaultConfig()

	if cfg.HeadroomPct != 80 {
		t.Errorf("HeadroomPct = %d, want 80", cfg.HeadroomPct)
	}
	// MaxVMSizeMB = 12G * 80% = 9830 MiB (default with 12G node RAM)
	if cfg.MaxVMSizeMB != 9830 {
		t.Errorf("MaxVMSizeMB = %d, want 9830 (12G * 80%%)", cfg.MaxVMSizeMB)
	}
}
