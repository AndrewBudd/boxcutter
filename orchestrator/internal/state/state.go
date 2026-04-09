package state

import (
	"sync"
	"time"
)

// Node represents a discovered compute node.
type Node struct {
	ID            string `json:"id"`
	TailscaleName string `json:"tailscale_name"`
	TailscaleIP   string `json:"tailscale_ip"`
	BridgeIP      string `json:"bridge_ip"`
	APIAddr       string `json:"api_addr"`
	Status        string `json:"status"`
	DiscoveredAt  string `json:"discovered_at"`
	LastSeen      string `json:"last_seen"`

	// Transient fields populated from node health queries
	RAMTotalMIB     int `json:"ram_total_mib,omitempty"`
	RAMAllocatedMIB int `json:"ram_allocated_mib,omitempty"`
	RAMAvailableMIB int `json:"ram_available_mib,omitempty"`
	DiskTotalMB     int `json:"disk_total_mb,omitempty"`
	DiskUsedMB      int `json:"disk_used_mb,omitempty"`
	DiskVMsMB       int `json:"disk_vms_mb,omitempty"`
	VMsRunning      int `json:"vms_running,omitempty"`
}

// VM is a lightweight record mapping a VM name to its node.
type VM struct {
	Name   string `json:"name"`
	NodeID string `json:"node_id"`
	Status string `json:"status"`
}

// GoldenImage tracks which golden image versions exist on which nodes.
type GoldenImage struct {
	Version      string `json:"version"`
	NodeID       string `json:"node_id"`
	DiscoveredAt string `json:"discovered_at"`
}

// Store is a thread-safe in-memory store for cluster state.
// It replaces SQLite for nodes, VMs, and golden images.
// The orchestrator rebuilds this from node queries on every startup.
type Store struct {
	mu           sync.RWMutex
	nodes        map[string]*Node        // nodeID -> Node
	vms          map[string]*VM          // vmName -> VM
	goldenImages map[string][]GoldenImage // version -> []GoldenImage
}

// New creates an empty state store.
func New() *Store {
	return &Store{
		nodes:        make(map[string]*Node),
		vms:          make(map[string]*VM),
		goldenImages: make(map[string][]GoldenImage),
	}
}

// --- Node operations ---

// SetNode adds or updates a node.
func (s *Store) SetNode(n *Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.ID] = n
}

// GetNode returns a node by ID, or nil if not found.
func (s *Store) GetNode(id string) *Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[id]
	if !ok {
		return nil
	}
	copy := *n
	return &copy
}

// ListNodes returns all nodes sorted by ID.
func (s *Store) ListNodes() []*Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	nodes := make([]*Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		copy := *n
		nodes = append(nodes, &copy)
	}
	return nodes
}

// ActiveNodes returns nodes with status "active".
func (s *Store) ActiveNodes() []*Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var nodes []*Node
	for _, n := range s.nodes {
		if n.Status == "active" {
			copy := *n
			nodes = append(nodes, &copy)
		}
	}
	return nodes
}

// SetNodeStatus updates a node's status.
func (s *Store) SetNodeStatus(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.nodes[id]; ok {
		n.Status = status
	}
}

// UpdateNodeLastSeen updates the node's last-seen timestamp.
func (s *Store) UpdateNodeLastSeen(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.nodes[id]; ok {
		n.LastSeen = time.Now().Format(time.RFC3339)
	}
}

// DeleteNode removes a node from the store.
func (s *Store) DeleteNode(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, id)
}

// --- VM operations ---

// SetVM adds or updates a VM.
func (s *Store) SetVM(v *VM) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vms[v.Name] = v
}

// GetVM returns a VM by name, or nil if not found.
func (s *Store) GetVM(name string) *VM {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.vms[name]
	if !ok {
		return nil
	}
	copy := *v
	return &copy
}

// ListVMs returns all VMs.
func (s *Store) ListVMs() []*VM {
	s.mu.RLock()
	defer s.mu.RUnlock()
	vms := make([]*VM, 0, len(s.vms))
	for _, v := range s.vms {
		copy := *v
		vms = append(vms, &copy)
	}
	return vms
}

// DeleteVM removes a VM from the store.
func (s *Store) DeleteVM(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vms, name)
}

// UpdateVMStatus updates a VM's status.
func (s *Store) UpdateVMStatus(name, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vms[name]; ok {
		v.Status = status
	}
}

// SyncNodeVMs replaces the VM records for a node with the given list.
// VMs on other nodes are not affected.
func (s *Store) SyncNodeVMs(nodeID string, vms []VM) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove existing VMs for this node
	for name, v := range s.vms {
		if v.NodeID == nodeID {
			delete(s.vms, name)
		}
	}

	// Add the current VMs
	for i := range vms {
		s.vms[vms[i].Name] = &VM{
			Name:   vms[i].Name,
			NodeID: vms[i].NodeID,
			Status: vms[i].Status,
		}
	}
}

// --- Golden image operations ---

// SetGoldenImages replaces all golden image records for a node.
func (s *Store) SetGoldenImages(nodeID string, versions []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Format(time.RFC3339)

	// Remove existing entries for this node from all versions
	for ver, images := range s.goldenImages {
		var kept []GoldenImage
		for _, img := range images {
			if img.NodeID != nodeID {
				kept = append(kept, img)
			}
		}
		if len(kept) == 0 {
			delete(s.goldenImages, ver)
		} else {
			s.goldenImages[ver] = kept
		}
	}

	// Add new entries
	for _, ver := range versions {
		s.goldenImages[ver] = append(s.goldenImages[ver], GoldenImage{
			Version:      ver,
			NodeID:       nodeID,
			DiscoveredAt: now,
		})
	}
}

// ListGoldenImages returns all golden images.
func (s *Store) ListGoldenImages() []GoldenImage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var all []GoldenImage
	for _, images := range s.goldenImages {
		all = append(all, images...)
	}
	return all
}
