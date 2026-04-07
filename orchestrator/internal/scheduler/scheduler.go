package scheduler

import (
	"fmt"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
)

// effectiveFreeRAM returns the best estimate of free RAM on a node.
// Prefers actual MemAvailable (from /proc/meminfo) when reported by the node,
// falls back to declared (total - allocated) otherwise.
func effectiveFreeRAM(n *db.Node) int {
	if n.RAMAvailableMIB > 0 {
		return n.RAMAvailableMIB
	}
	return n.RAMTotalMIB - n.RAMAllocatedMIB
}

// PickNode selects the best active node for a new VM based on available RAM.
// Uses actual MemAvailable when reported by nodes, falls back to declared allocation.
func PickNode(nodes []*db.Node, requiredRAM int) (*db.Node, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no active nodes available")
	}

	var best *db.Node
	var bestFree int

	for _, n := range nodes {
		if n.Status != "active" {
			continue
		}
		free := effectiveFreeRAM(n)
		if free >= requiredRAM && (best == nil || free > bestFree) {
			best = n
			bestFree = free
		}
	}

	if best == nil {
		// Fall back to node with most free RAM even if insufficient
		for _, n := range nodes {
			if n.Status != "active" {
				continue
			}
			free := effectiveFreeRAM(n)
			if best == nil || free > bestFree {
				best = n
				bestFree = free
			}
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no active nodes available")
	}
	return best, nil
}
