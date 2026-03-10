// Package cluster manages the persistent cluster state (cluster.json).
package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const DefaultStatePath = "/var/lib/boxcutter/cluster.json"

type VMEntry struct {
	ID       string `json:"id"`
	Type     string `json:"type"`                 // "orchestrator" or "node"
	Status   string `json:"status,omitempty"`      // "active", "draining", "upgrading"; empty = active
	BridgeIP string `json:"bridge_ip"`
	Disk     string `json:"disk"`
	ISO      string `json:"iso"`
	PID      int    `json:"pid,omitempty"`
	VCPU     int    `json:"vcpu"`
	RAM      string `json:"ram"`
	TAP      string `json:"tap"`
	MAC      string `json:"mac"`

	// OCI image tracking (set when VM was created from a pulled image)
	ImageVersion string `json:"image_version,omitempty"` // e.g., "v0.1.0"
	ImageCommit  string `json:"image_commit,omitempty"`  // e.g., "049616f"
	ImageDigest  string `json:"image_digest,omitempty"`  // OCI manifest digest
}

// IsActive returns true when the VM should be treated as active (not being drained/upgraded).
func (e *VMEntry) IsActive() bool {
	return e.Status == "" || e.Status == "active"
}

// UpgradeIntent records an in-progress upgrade so it can be resumed after a crash.
type UpgradeIntent struct {
	VMType    string   `json:"vm_type"`              // "node", "orchestrator", "all"
	Tag       string   `json:"tag"`                  // OCI image tag
	StartedAt string   `json:"started_at"`           // RFC3339
	Phase     string   `json:"phase"`                // "pulling", "launching", "draining", "complete", "failed"
	BasePath  string   `json:"base_path,omitempty"`  // path to base QCOW2 after pull

	// Node upgrades: old nodes still needing replacement
	PendingNodes []string `json:"pending_nodes,omitempty"`

	// Orchestrator upgrades: new orchestrator's temp bridge IP
	NewOrchIP string `json:"new_orch_ip,omitempty"`

	// Image metadata from pull
	ImageVersion string `json:"image_version,omitempty"`
	ImageCommit  string `json:"image_commit,omitempty"`
	ImageDigest  string `json:"image_digest,omitempty"`
}

type State struct {
	Orchestrator  *VMEntry       `json:"orchestrator,omitempty"`
	Nodes         []VMEntry      `json:"nodes"`
	UpgradeIntent *UpgradeIntent `json:"upgrade_intent,omitempty"`

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

// Save atomically writes the state to disk using write-fsync-rename.
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
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}

	// Sync parent directory to persist the rename
	d, err := os.Open(dir)
	if err != nil {
		return nil // non-fatal: file is already renamed
	}
	d.Sync()
	d.Close()
	return nil
}

func (s *State) SetOrchestrator(entry VMEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.Type = "orchestrator"
	if entry.Status == "" {
		entry.Status = "active"
	}
	s.Orchestrator = &entry
}

func (s *State) AddNode(entry VMEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.Type = "node"
	if entry.Status == "" {
		entry.Status = "active"
	}
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

// ActiveNodeCount returns the number of nodes not being drained/upgraded.
func (s *State) ActiveNodeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, n := range s.Nodes {
		if n.IsActive() {
			count++
		}
	}
	return count
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

// SetNodeStatus sets the status field on a VM entry (node or orchestrator).
func (s *State) SetNodeStatus(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Orchestrator != nil && s.Orchestrator.ID == id {
		s.Orchestrator.Status = status
		return
	}
	for i, n := range s.Nodes {
		if n.ID == id {
			s.Nodes[i].Status = status
			return
		}
	}
}

func (s *State) SetUpgradeIntent(intent *UpgradeIntent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if intent != nil && intent.StartedAt == "" {
		intent.StartedAt = time.Now().Format(time.RFC3339)
	}
	s.UpgradeIntent = intent
}

func (s *State) ClearUpgradeIntent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.UpgradeIntent = nil
}
