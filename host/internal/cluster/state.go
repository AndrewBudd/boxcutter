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

// MatchesImage returns true if this VM was created from the given image.
// Compares by digest first (immutable), falls back to version string.
func (e *VMEntry) MatchesImage(ref *ImageRef) bool {
	if ref == nil {
		return false
	}
	if e.ImageDigest != "" && ref.Digest != "" {
		return e.ImageDigest == ref.Digest
	}
	if e.ImageVersion != "" && ref.Version != "" {
		return e.ImageVersion == ref.Version
	}
	return false
}

// ImageRef holds resolved OCI image metadata for an upgrade goal.
type ImageRef struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Digest  string `json:"digest"`
}

// UpgradeGoal declares the desired image version for VMs. The reconciler
// observes the running cluster and takes one step toward this goal each
// time it is called. When all VMs match the goal, it is cleared.
type UpgradeGoal struct {
	VMType string `json:"vm_type"` // "node", "orchestrator", "all"
	Tag    string `json:"tag"`     // OCI image tag (e.g., "latest", "v0.6.0")

	// Resolved image metadata (populated after pull)
	NodeImage *ImageRef `json:"node_image,omitempty"`
	OrchImage *ImageRef `json:"orch_image,omitempty"`

	// Base QCOW2 paths (populated after pull + decompress)
	NodeBasePath string `json:"node_base_path,omitempty"`
	OrchBasePath string `json:"orch_base_path,omitempty"`

	// Node upgrade: initial count at goal creation, used to detect surplus
	InitialNodeCount int `json:"initial_node_count,omitempty"`

	// Node upgrade: ID of the replacement node that has received the latest agent binary.
	// Reset when a new replacement is launched.
	DeployedNodeID string `json:"deployed_node_id,omitempty"`

	// Orchestrator upgrade: temp bridge IP/TAP/MAC for new orchestrator
	NewOrchIP  string `json:"new_orch_ip,omitempty"`
	NewOrchTAP string `json:"new_orch_tap,omitempty"`
	NewOrchMAC string `json:"new_orch_mac,omitempty"`

	CreatedAt string `json:"created_at"`
}

type State struct {
	Orchestrator *VMEntry     `json:"orchestrator,omitempty"`
	Nodes        []VMEntry    `json:"nodes"`
	UpgradeGoal  *UpgradeGoal `json:"upgrade_goal,omitempty"`

	// Legacy field — read for migration, never written.
	LegacyUpgradeIntent *legacyUpgradeIntent `json:"upgrade_intent,omitempty"`

	mu   sync.RWMutex
	path string
}

// legacyUpgradeIntent is the old imperative FSM state. Kept only for
// reading old cluster.json files during migration.
type legacyUpgradeIntent struct {
	VMType   string `json:"vm_type"`
	Tag      string `json:"tag"`
	Phase    string `json:"phase"`
	BasePath string `json:"base_path,omitempty"`
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

	// Migrate legacy UpgradeIntent → UpgradeGoal
	if s.LegacyUpgradeIntent != nil && s.UpgradeGoal == nil {
		old := s.LegacyUpgradeIntent
		if old.Phase != "complete" && old.Phase != "failed" {
			s.UpgradeGoal = &UpgradeGoal{
				VMType:    old.VMType,
				Tag:       old.Tag,
				CreatedAt: time.Now().Format(time.RFC3339),
			}
			// If the old intent had a base path and it still exists, carry it forward
			if old.BasePath != "" {
				if _, err := os.Stat(old.BasePath); err == nil {
					if old.VMType == "node" {
						s.UpgradeGoal.NodeBasePath = old.BasePath
					} else if old.VMType == "orchestrator" {
						s.UpgradeGoal.OrchBasePath = old.BasePath
					}
				}
			}
		}
		s.LegacyUpgradeIntent = nil
		// Revert any non-active nodes from old FSM state
		for i := range s.Nodes {
			if !s.Nodes[i].IsActive() {
				s.Nodes[i].Status = "active"
			}
		}
		if s.Orchestrator != nil && !s.Orchestrator.IsActive() {
			s.Orchestrator.Status = "active"
		}
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

// FindNodeWithStatus returns the first node with the given status, or nil.
func (s *State) FindNodeWithStatus(status string) *VMEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, n := range s.Nodes {
		if n.Status == status {
			cp := n
			return &cp
		}
	}
	return nil
}

func (s *State) SetUpgradeGoal(goal *UpgradeGoal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if goal != nil && goal.CreatedAt == "" {
		goal.CreatedAt = time.Now().Format(time.RFC3339)
	}
	s.UpgradeGoal = goal
}

func (s *State) ClearUpgradeGoal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.UpgradeGoal = nil
}
