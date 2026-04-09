// Package collab provides cross-agent collaboration: team messaging,
// file conflict detection, and a shared team scratchpad.
package collab

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// TeamMessage is a structured message between agents.
type TeamMessage struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to,omitempty"`        // empty = broadcast to team
	Team      string `json:"team"`
	Type      string `json:"type"`                // "notification", "request", "conflict_alert", "decision", "context_share"
	Subject   string `json:"subject,omitempty"`
	Body      string `json:"body"`
	ReplyTo   string `json:"reply_to,omitempty"`   // correlation ID for request/reply
	Timestamp string `json:"timestamp"`
}

// FileReport is a snapshot of which files an agent is modifying.
type FileReport struct {
	Agent     string   `json:"agent"`
	Team      string   `json:"team"`
	Branch    string   `json:"branch"`
	Files     []string `json:"files"`
	Timestamp string   `json:"timestamp"`
}

// FileConflict indicates two agents editing the same files.
type FileConflict struct {
	File    string `json:"file"`
	AgentA  string `json:"agent_a"`
	BranchA string `json:"branch_a"`
	AgentB  string `json:"agent_b"`
	BranchB string `json:"branch_b"`
}

// ScratchpadEntry is an entry in the shared team scratchpad.
type ScratchpadEntry struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	Type      string `json:"type"` // "decision", "discovery", "warning", "blocker"
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	Timestamp string `json:"timestamp"`
}

// Hub manages collaboration state for all teams.
type Hub struct {
	mu          sync.RWMutex
	messages    map[string][]*TeamMessage     // team -> pending messages (keyed by target agent)
	files       map[string]*FileReport        // vm_name -> latest file report
	scratchpads map[string][]*ScratchpadEntry // team -> entries
}

// NewHub creates a collaboration hub.
func NewHub() *Hub {
	return &Hub{
		messages:    make(map[string][]*TeamMessage),
		files:       make(map[string]*FileReport),
		scratchpads: make(map[string][]*ScratchpadEntry),
	}
}

// SendMessage delivers a message to a specific agent or broadcasts to a team.
func (h *Hub) SendMessage(msg *TeamMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if msg.ID == "" {
		msg.ID = fmt.Sprintf("tm-%d", time.Now().UnixNano())
	}
	if msg.Timestamp == "" {
		msg.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	key := msg.Team
	h.messages[key] = append(h.messages[key], msg)
}

// PendingMessages returns messages for a specific agent, consuming them.
func (h *Hub) PendingMessages(team, agentVM string) []*TeamMessage {
	h.mu.Lock()
	defer h.mu.Unlock()

	all := h.messages[team]
	var pending []*TeamMessage
	var remaining []*TeamMessage

	for _, m := range all {
		if m.To == "" || m.To == agentVM {
			pending = append(pending, m)
		} else {
			remaining = append(remaining, m)
		}
	}
	h.messages[team] = remaining
	return pending
}

// ListMessages returns all messages for a team (non-destructive, for CLI display).
func (h *Hub) ListMessages(team string) []*TeamMessage {
	h.mu.RLock()
	defer h.mu.RUnlock()
	msgs := make([]*TeamMessage, len(h.messages[team]))
	copy(msgs, h.messages[team])
	return msgs
}

// ReportFiles stores which files an agent is modifying.
func (h *Hub) ReportFiles(report *FileReport) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if report.Timestamp == "" {
		report.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	h.files[report.Agent] = report
}

// DetectConflicts checks for file overlaps between agents on the same team.
func (h *Hub) DetectConflicts(team string) []FileConflict {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Collect file reports for this team
	var reports []*FileReport
	for _, r := range h.files {
		if r.Team == team && len(r.Files) > 0 {
			reports = append(reports, r)
		}
	}

	// Find overlaps
	var conflicts []FileConflict
	for i := 0; i < len(reports); i++ {
		for j := i + 1; j < len(reports); j++ {
			for _, fA := range reports[i].Files {
				for _, fB := range reports[j].Files {
					if fA == fB {
						conflicts = append(conflicts, FileConflict{
							File:    fA,
							AgentA:  reports[i].Agent,
							BranchA: reports[i].Branch,
							AgentB:  reports[j].Agent,
							BranchB: reports[j].Branch,
						})
					}
				}
			}
		}
	}
	return conflicts
}

// GetFiles returns all file reports for a team.
func (h *Hub) GetFiles(team string) []*FileReport {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var reports []*FileReport
	for _, r := range h.files {
		if r.Team == team {
			reports = append(reports, r)
		}
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].Agent < reports[j].Agent })
	return reports
}

// AppendScratchpad adds an entry to a team's scratchpad.
func (h *Hub) AppendScratchpad(team string, entry *ScratchpadEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if entry.ID == "" {
		entry.ID = fmt.Sprintf("sp-%d", time.Now().UnixNano())
	}
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	h.scratchpads[team] = append(h.scratchpads[team], entry)

	// Keep last 100 entries per team
	if len(h.scratchpads[team]) > 100 {
		h.scratchpads[team] = h.scratchpads[team][len(h.scratchpads[team])-100:]
	}
}

// GetScratchpad returns a team's scratchpad entries.
func (h *Hub) GetScratchpad(team string) []*ScratchpadEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	entries := make([]*ScratchpadEntry, len(h.scratchpads[team]))
	copy(entries, h.scratchpads[team])
	return entries
}

// FormatConflictAlert builds a tapegun inbox message for a conflict alert.
func FormatConflictAlert(conflict FileConflict, targetAgent string) string {
	var other, otherBranch string
	if targetAgent == conflict.AgentA {
		other = conflict.AgentB
		otherBranch = conflict.BranchB
	} else {
		other = conflict.AgentA
		otherBranch = conflict.BranchA
	}
	return fmt.Sprintf("CONFLICT ALERT: %s is also editing %s (branch: %s). Coordinate to avoid merge conflicts.",
		other, conflict.File, otherBranch)
}

// FormatConflictSummary builds a multi-file conflict summary.
func FormatConflictSummary(conflicts []FileConflict, targetAgent string) string {
	if len(conflicts) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("FILE CONFLICT DETECTED\n\n")

	seen := make(map[string]bool)
	for _, c := range conflicts {
		key := c.File + "|" + c.AgentA + "|" + c.AgentB
		if seen[key] {
			continue
		}
		seen[key] = true

		var other, otherBranch string
		if targetAgent == c.AgentA {
			other = c.AgentB
			otherBranch = c.BranchB
		} else {
			other = c.AgentA
			otherBranch = c.BranchA
		}
		fmt.Fprintf(&b, "- %s: also being edited by %s (branch: %s)\n", c.File, other, otherBranch)
	}

	b.WriteString("\nCoordinate with the other agent to avoid merge conflicts. Consider working on different sections or rebasing.")
	return b.String()
}
