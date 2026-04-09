package main

import "testing"

func TestMemPressureCandidate(t *testing.T) {
	tests := []struct {
		name         string
		nodes        []nodeCapacity
		thresholdPct int
		wantNodeID   string
		wantAbove    bool // whether any node should exceed threshold
	}{
		{
			name: "no pressure — all nodes healthy",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 12000, ramAvailableMiB: 6000}, // 50% used
				{nodeID: "node-2", totalRAM: 12000, ramAvailableMiB: 4000}, // 67% used
			},
			thresholdPct: 90,
			wantNodeID:   "",
			wantAbove:    false,
		},
		{
			name: "one node under pressure",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 12000, ramAvailableMiB: 6000}, // 50% used
				{nodeID: "node-2", totalRAM: 12000, ramAvailableMiB: 1000}, // 92% used
			},
			thresholdPct: 90,
			wantNodeID:   "node-2",
			wantAbove:    true,
		},
		{
			name: "multiple nodes under pressure picks worst",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 12000, ramAvailableMiB: 800},  // 93% used
				{nodeID: "node-2", totalRAM: 12000, ramAvailableMiB: 500},  // 96% used
				{nodeID: "node-3", totalRAM: 12000, ramAvailableMiB: 6000}, // 50% used
			},
			thresholdPct: 90,
			wantNodeID:   "node-2", // least available
			wantAbove:    true,
		},
		{
			name: "exactly at threshold triggers",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 10000, ramAvailableMiB: 1000}, // exactly 90% used
			},
			thresholdPct: 90,
			wantNodeID:   "node-1",
			wantAbove:    true,
		},
		{
			name: "just below threshold — no trigger",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 10000, ramAvailableMiB: 1100}, // 89% used
			},
			thresholdPct: 90,
			wantNodeID:   "",
			wantAbove:    false,
		},
		{
			name:         "empty nodes list",
			nodes:        nil,
			thresholdPct: 90,
			wantNodeID:   "",
			wantAbove:    false,
		},
		{
			name: "node with zero totalRAM skipped",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 0, ramAvailableMiB: 100},
			},
			thresholdPct: 90,
			wantNodeID:   "",
			wantAbove:    false,
		},
		{
			name: "node with zero ramAvailableMiB skipped",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 12000, ramAvailableMiB: 0},
			},
			thresholdPct: 90,
			wantNodeID:   "",
			wantAbove:    false,
		},
		{
			name: "OOM scenario — 18G declared on 12G node",
			nodes: []nodeCapacity{
				{nodeID: "node-16", totalRAM: 12288, usedRAM: 18432, ramAvailableMiB: 200}, // nearly OOM
			},
			thresholdPct: 90,
			wantNodeID:   "node-16",
			wantAbove:    true,
		},
		{
			name: "custom threshold 80%",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 10000, ramAvailableMiB: 1500}, // 85% used
			},
			thresholdPct: 80,
			wantNodeID:   "node-1",
			wantAbove:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotPct := memPressureCandidate(tt.nodes, tt.thresholdPct)
			if gotID != tt.wantNodeID {
				t.Errorf("memPressureCandidate() nodeID = %q, want %q", gotID, tt.wantNodeID)
			}
			if tt.wantAbove && gotPct < tt.thresholdPct {
				t.Errorf("memPressureCandidate() pct = %d, want >= %d", gotPct, tt.thresholdPct)
			}
			if !tt.wantAbove && gotID != "" {
				t.Errorf("memPressureCandidate() should return empty nodeID when below threshold, got %q", gotID)
			}
		})
	}
}
