package scheduler

import (
	"fmt"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
)

// HeadroomPercent is the capacity threshold above which nodes are deprioritized.
// Nodes above this percentage of RAM utilization are considered "hot" and will
// only be used if no other node can fit the VM.
const HeadroomPercent = 80

// effectiveFreeRAM returns the best estimate of free RAM on a node.
// Prefers actual MemAvailable (from /proc/meminfo) when reported by the node,
// falls back to declared (total - allocated) otherwise.
func effectiveFreeRAM(n *db.Node) int {
	if n.RAMAvailableMIB > 0 {
		return n.RAMAvailableMIB
	}
	return n.RAMTotalMIB - n.RAMAllocatedMIB
}

// utilizationPercent returns the percentage of RAM used on a node (0-100).
func utilizationPercent(n *db.Node) int {
	if n.RAMTotalMIB == 0 {
		return 100
	}
	used := n.RAMTotalMIB - effectiveFreeRAM(n)
	return (used * 100) / n.RAMTotalMIB
}

// PickNode selects the best active node for a new VM using a spread policy.
//
// Strategy:
//  1. Filter to active nodes with enough free RAM for the requested VM.
//  2. Among those, prefer nodes below the headroom threshold (80% utilization).
//  3. Within each tier, pick the node with the most free RAM (least loaded).
//  4. If no node has enough RAM, fall back to the node with the most free RAM.
func PickNode(nodes []*db.Node, requiredRAM int) (*db.Node, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no active nodes available")
	}

	var (
		bestCool     *db.Node // below headroom, has capacity
		bestCoolFree int
		bestHot      *db.Node // above headroom, has capacity
		bestHotFree  int
		bestAny      *db.Node // fallback: most free RAM regardless
		bestAnyFree  int
	)

	for _, n := range nodes {
		if n.Status != "active" {
			continue
		}
		free := effectiveFreeRAM(n)

		// Track best fallback node regardless of capacity
		if bestAny == nil || free > bestAnyFree {
			bestAny = n
			bestAnyFree = free
		}

		if free < requiredRAM {
			continue
		}

		if utilizationPercent(n) < HeadroomPercent {
			// Cool node — below headroom threshold
			if bestCool == nil || free > bestCoolFree {
				bestCool = n
				bestCoolFree = free
			}
		} else {
			// Hot node — above headroom threshold
			if bestHot == nil || free > bestHotFree {
				bestHot = n
				bestHotFree = free
			}
		}
	}

	// Prefer cool nodes, then hot nodes, then fallback
	if bestCool != nil {
		return bestCool, nil
	}
	if bestHot != nil {
		return bestHot, nil
	}
	if bestAny != nil {
		return bestAny, nil
	}
	return nil, fmt.Errorf("no active nodes available")
}
