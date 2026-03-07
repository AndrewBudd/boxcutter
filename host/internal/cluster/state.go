// Package cluster manages the persistent cluster state (cluster.json).
package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const DefaultStatePath = "/var/lib/boxcutter/cluster.json"

type VMEntry struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "orchestrator" or "node"
	BridgeIP string `json:"bridge_ip"`
	Disk     string `json:"disk"`
	ISO      string `json:"iso"`
	PID      int    `json:"pid,omitempty"`
	VCPU     int    `json:"vcpu"`
	RAM      string `json:"ram"`
	TAP      string `json:"tap"`
	MAC      string `json:"mac"`
}

type State struct {
	Orchestrator *VMEntry  `json:"orchestrator,omitempty"`
	Nodes        []VMEntry `json:"nodes"`

	mu   sync.RWMutex
	path string
}

func Load(path string) (*State, error) {
	s := &State{path: path, Nodes: []VMEntry{}}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading cluster state: %w", err)
	}

	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parsing cluster state: %w", err)
	}
	if s.Nodes == nil {
		s.Nodes = []VMEntry{}
	}

	return s, nil
}

func (s *State) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *State) SetOrchestrator(entry VMEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.Type = "orchestrator"
	s.Orchestrator = &entry
}

func (s *State) AddNode(entry VMEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.Type = "node"
	// Replace if exists
	for i, n := range s.Nodes {
		if n.ID == entry.ID {
			s.Nodes[i] = entry
			return
		}
	}
	s.Nodes = append(s.Nodes, entry)
}

func (s *State) RemoveNode(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, n := range s.Nodes {
		if n.ID == id {
			s.Nodes = append(s.Nodes[:i], s.Nodes[i+1:]...)
			return
		}
	}
}

func (s *State) GetNode(id string) *VMEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, n := range s.Nodes {
		if n.ID == id {
			return &n
		}
	}
	return nil
}

func (s *State) NodeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Nodes)
}

func (s *State) NextNodeNum() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	max := 0
	for _, n := range s.Nodes {
		// Extract number from "boxcutter-node-N"
		var num int
		if _, err := fmt.Sscanf(n.ID, "boxcutter-node-%d", &num); err == nil {
			if num > max {
				max = num
			}
		}
	}
	return max + 1
}

func (s *State) SetPID(id string, pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Orchestrator != nil && s.Orchestrator.ID == id {
		s.Orchestrator.PID = pid
		return
	}
	for i, n := range s.Nodes {
		if n.ID == id {
			s.Nodes[i].PID = pid
			return
		}
	}
}
