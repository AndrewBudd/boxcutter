package main

import "testing"

func TestScaleDownCandidate(t *testing.T) {
	tests := []struct {
		name         string
		nodes        []nodeCapacity
		totalRAM     int
		usedRAM      int
		scaleDownPct int
		scaleUpPct   int
		wantNodeID   string
	}{
		{
			name:         "single node never scales down",
			nodes:        []nodeCapacity{{nodeID: "node-1", totalRAM: 10000, usedRAM: 1000}},
			totalRAM:     10000,
			usedRAM:      1000,
			scaleDownPct: 30,
			scaleUpPct:   80,
			wantNodeID:   "",
		},
		{
			name:         "above threshold does not scale down",
			nodes:        []nodeCapacity{{nodeID: "node-1", totalRAM: 10000, usedRAM: 4000}, {nodeID: "node-2", totalRAM: 10000, usedRAM: 3000}},
			totalRAM:     20000,
			usedRAM:      7000, // 35% > 30%
			scaleDownPct: 30,
			scaleUpPct:   80,
			wantNodeID:   "",
		},
		{
			name: "below threshold picks least loaded node",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 10000, usedRAM: 2000},
				{nodeID: "node-2", totalRAM: 10000, usedRAM: 500},
				{nodeID: "node-3", totalRAM: 10000, usedRAM: 1500},
			},
			totalRAM:     30000,
			usedRAM:      4000, // 13% < 30%
			scaleDownPct: 30,
			scaleUpPct:   80,
			wantNodeID:   "node-2", // least loaded
		},
		{
			name: "would exceed scale-up threshold after removal",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 10000, usedRAM: 2500},
				{nodeID: "node-2", totalRAM: 10000, usedRAM: 2500},
			},
			totalRAM:     20000,
			usedRAM:      5000, // 25% < 30%, but after removing a node: 5000/10000 = 50% < 80%
			scaleDownPct: 30,
			scaleUpPct:   80,
			wantNodeID:   "node-1", // either node could be picked (both equal), first wins
		},
		{
			name: "would exceed scale-up threshold blocks scale-down",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 5000, usedRAM: 1000},
				{nodeID: "node-2", totalRAM: 5000, usedRAM: 1500},
			},
			totalRAM:     10000,
			usedRAM:      2500, // 25% < 30%, but after removing: 2500/5000 = 50%
			scaleDownPct: 30,
			scaleUpPct:   50, // would hit exactly 50% = scaleUpPct, should block
			wantNodeID:   "",
		},
		{
			name: "three nodes, safe to remove one",
			nodes: []nodeCapacity{
				{nodeID: "node-1", totalRAM: 10000, usedRAM: 1000},
				{nodeID: "node-2", totalRAM: 10000, usedRAM: 500},
				{nodeID: "node-3", totalRAM: 10000, usedRAM: 500},
			},
			totalRAM:     30000,
			usedRAM:      2000, // 6.6% < 30%, after removing node-2: 2000/20000 = 10% < 80%
			scaleDownPct: 30,
			scaleUpPct:   80,
			wantNodeID:   "node-2", // tied at 500, first one wins
		},
		{
			name:         "zero total RAM",
			nodes:        []nodeCapacity{{nodeID: "node-1"}, {nodeID: "node-2"}},
			totalRAM:     0,
			usedRAM:      0,
			scaleDownPct: 30,
			scaleUpPct:   80,
			wantNodeID:   "",
		},
		{
			name:         "no nodes",
			nodes:        nil,
			totalRAM:     0,
			usedRAM:      0,
			scaleDownPct: 30,
			scaleUpPct:   80,
			wantNodeID:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scaleDownCandidate(tt.nodes, tt.totalRAM, tt.usedRAM, tt.scaleDownPct, tt.scaleUpPct)
			if got != tt.wantNodeID {
				t.Errorf("scaleDownCandidate() = %q, want %q", got, tt.wantNodeID)
			}
		})
	}
}
