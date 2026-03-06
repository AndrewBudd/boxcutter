package scheduler

import (
	"fmt"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
)

// PickNode selects the best active node for a new VM based on available RAM.
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
		free := n.RAMTotalMIB - n.RAMAllocatedMIB
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
			free := n.RAMTotalMIB - n.RAMAllocatedMIB
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
