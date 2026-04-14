package registry

import (
	"encoding/json"
	"sync"
	"time"
)

// ActivityReport is a snapshot of a VM's current state, posted by the guest daemon.
type ActivityReport struct {
	Timestamp   time.Time `json:"timestamp"`
	PaneContent string    `json:"pane_content"`
	Status      string    `json:"status"`
	Summary     string    `json:"summary,omitempty"`
}

// StatusReport is a self-reported status from Claude Code inside the VM.
type StatusReport struct {
	Timestamp time.Time `json:"timestamp"`
	Status    string    `json:"status"` // last assistant message or summary
}

// HealthReport is a periodic health check from the in-VM agent.
type HealthReport struct {
	Timestamp    time.Time         `json:"timestamp"`
	Health       string            `json:"health"` // "healthy", "degraded", or "unhealthy"
	HealthChecks map[string]bool   `json:"health_checks"`
	Details      map[string]string `json:"details,omitempty"`
}

// IsHealthy returns true if the health status is "healthy".
func (h *HealthReport) IsHealthy() bool {
	return h != nil && h.Health == "healthy"
}

// MailboxMessage is a VM-to-VM message stored in the recipient's mailbox.
type MailboxMessage struct {
	ID        string     `json:"id"`
	From      string     `json:"from"`
	To        string     `json:"to"`
	Subject   string     `json:"subject,omitempty"`
	Body      string     `json:"body"`
	CreatedAt time.Time  `json:"created_at"`
	InFlight  *time.Time `json:"in_flight,omitempty"` // when consumed; nil = pending
}

// InFlightTimeout is how long a consumed message stays invisible before redelivery.
const InFlightTimeout = 30 * time.Second

// Message is a directive sent to a VM by an external tapegun agent.
type Message struct {
	ID        string     `json:"id"`
	From      string     `json:"from"`
	Body      string     `json:"body"`
	Priority  string     `json:"priority"`
	SendKeys  bool       `json:"send_keys,omitempty"`
	NoEnter   bool       `json:"no_enter,omitempty"` // send_keys without pressing Enter (paste-only)
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
}

// VMActivitySummary is a view of a VM's tapegun state including memory context.
type VMActivitySummary struct {
	VMID            string            `json:"vm_id"`
	LastActivity    *ActivityReport   `json:"last_activity,omitempty"`
	LastStatus      *StatusReport     `json:"last_status,omitempty"`
	LastFiles       *FileTracking     `json:"last_files,omitempty"`
	LastCheckpoint  *CheckpointData   `json:"last_checkpoint,omitempty"`
	AgentConfig     *AgentConfig      `json:"agent_config,omitempty"`
	PendingMessages int               `json:"pending_messages"`
}

// PersonaConfig describes the persona files and instructions for the in-VM agent.
type PersonaConfig struct {
	Role         string `json:"role,omitempty"`         // e.g., "backend-eng", "security-reviewer"
	ClaudeMD     string `json:"claude_md,omitempty"`    // contents of persona CLAUDE.md
	Instructions string `json:"instructions,omitempty"` // additional instructions for the agent
}

// VMConfigInfo describes the VM resource configuration requested by the team spec.
type VMConfigInfo struct {
	VMType string `json:"vm_type,omitempty"` // "firecracker" or "qemu"
	CPUs   int    `json:"cpus,omitempty"`
	MemMB  int    `json:"mem_mb,omitempty"`
	DiskGB int    `json:"disk_gb,omitempty"`
}

// InferenceConfig describes the inference provider configuration for a VM.
// This is passed as part of AgentConfig from the orchestrator.
type InferenceConfig struct {
	Provider string `json:"provider,omitempty"` // "local", "openrouter", or "anthropic"
	Model    string `json:"model,omitempty"`    // e.g. "qwen3-coder:30b"
	Endpoint string `json:"endpoint,omitempty"` // Inference endpoint URL
	APIKey   string `json:"api_key,omitempty"`  // API key for cloud providers (e.g. OpenRouter)
}

// ClaudeConfig configures Claude Code authentication for VMs (#175).
type ClaudeConfig struct {
	Auth       string `json:"auth,omitempty"`        // "oauth-token", "api-key", "api-key-helper"
	OAuthToken string `json:"oauth_token,omitempty"` // OAuth refresh token from `claude setup-token`
	APIKey     string `json:"api_key,omitempty"`     // Anthropic API key
}

// AgentConfig is the boot agent specification for a VM, derived from the
// declarative team YAML. It tells the in-VM agent what persona to assume,
// which repos to clone, what tapegun sequences to run, and access policy.
type AgentConfig struct {
	Team             string            `json:"team,omitempty"`               // team name from team YAML
	Agent            string            `json:"agent,omitempty"`              // agent name within the team
	ReplicaIndex     int               `json:"replica_index,omitempty"`      // replica index for scaled agents
	Tool             string            `json:"tool,omitempty"`               // "claude-code", "opencode", "aider"
	Persona          *PersonaConfig    `json:"persona,omitempty"`            // persona configuration
	Repos            []string          `json:"repos,omitempty"`              // repos to clone on boot
	Tapegun          []string          `json:"tapegun,omitempty"`            // tapegun sequences to run on boot
	Access           []string          `json:"access,omitempty"`             // access policy entries
	Authorized       bool              `json:"authorized,omitempty"`         // whether the VM is pre-authorized
	VMConfig         *VMConfigInfo     `json:"vm_config,omitempty"`          // requested VM resource configuration
	Labels           map[string]string `json:"labels,omitempty"`             // labels for the VM
	TeamPersonaFiles []string          `json:"team_persona_files,omitempty"` // team-level persona file paths
	Inference        *InferenceConfig  `json:"inference,omitempty"`          // inference provider config
	Claude           *ClaudeConfig     `json:"claude,omitempty"`             // Claude Code auth config (#175)
	Plugins          *PluginsConfig    `json:"plugins,omitempty"`            // Claude Code plugin auto-install
}

// PluginsConfig specifies marketplaces to register and plugins to install.
type PluginsConfig struct {
	Marketplaces []string `json:"marketplaces,omitempty"` // GitHub repos or URLs
	Install      []string `json:"install,omitempty"`      // plugin@marketplace references
}

// CheckpointData holds a session checkpoint for conversation persistence.
// The session JSONL is stored as a file on disk (too large for in-memory registry);
// this struct tracks metadata and the file path.
type CheckpointData struct {
	SessionID  string `json:"session_id"`
	GitBranch  string `json:"git_branch,omitempty"`
	GitStash   string `json:"git_stash,omitempty"`
	Timestamp  string `json:"timestamp"`
	SizeBytes  int    `json:"size_bytes,omitempty"`
	FilePath   string `json:"-"` // internal: path to session JSONL on disk
}

// FileTracking records which files an agent is currently modifying.
type FileTracking struct {
	Branch    string   `json:"branch"`
	Files     []string `json:"files"`
	Timestamp string   `json:"timestamp"`
}

// ChannelEvent is a structured event pushed to a VM via the channel system.
// Events are delivered as <channel> tags in Claude Code's context.
type ChannelEvent struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`                // upgrade_starting, ci_failure, message, etc.
	Source    string            `json:"source"`              // "boxcutter", "ci-watcher", sender name
	Urgency   string            `json:"urgency,omitempty"`   // normal, high, critical
	Body      string            `json:"body"`                // human-readable event body
	Attrs     map[string]string `json:"attrs,omitempty"`     // typed attributes (pr_number, run_id, etc.)
	Timestamp string            `json:"timestamp"`
}

// ChannelReply is an outbound reply from an agent via the channel system.
type ChannelReply struct {
	ChatID    string `json:"chat_id,omitempty"` // correlates to inbound event ID
	Text      string `json:"text"`
	Status    string `json:"status,omitempty"`    // active, idle, blocked, etc.
	Summary   string `json:"summary,omitempty"`   // what the agent is working on
	Timestamp string `json:"timestamp"`
}

// channelSub is an SSE subscriber waiting for channel events.
type channelSub struct {
	ch   chan *ChannelEvent
	done chan struct{}
}

type VMRecord struct {
	VMID        string            `json:"vm_id"`
	VMType      string            `json:"vm_type,omitempty"` // "firecracker" or "qemu"
	IP          string            `json:"ip"`
	Mark        int               `json:"mark"`
	Mode        string            `json:"mode"`
	Labels      map[string]string `json:"labels,omitempty"`
	GitHubRepo     string            `json:"github_repo,omitempty"`     // backwards compat
	GitHubRepos    []string          `json:"github_repos,omitempty"`    // all repos
	GitHubProjects []string          `json:"github_projects,omitempty"` // authorized projects

	AgentConfig  *AgentConfig     `json:"agent_config,omitempty"`

	LastActivity   *ActivityReport   `json:"last_activity,omitempty"`
	LastStatus     *StatusReport     `json:"last_status,omitempty"`
	LastHealth     *HealthReport     `json:"last_health,omitempty"`
	LastCheckpoint *CheckpointData   `json:"last_checkpoint,omitempty"`
	LastFiles      *FileTracking     `json:"last_files,omitempty"`
	Inbox          []*Message        `json:"inbox,omitempty"`
	Mailbox        []*MailboxMessage `json:"mailbox,omitempty"`

	// Channel event subscribers (not serialized — ephemeral SSE connections)
	channelSubs []*channelSub `json:"-"`
	// Channel replies from the agent (collected by orchestrator)
	ChannelReplies []*ChannelReply `json:"channel_replies,omitempty"`
}

// AllGitHubRepos returns the list of repos, falling back to single GitHubRepo.
func (r *VMRecord) AllGitHubRepos() []string {
	if len(r.GitHubRepos) > 0 {
		return r.GitHubRepos
	}
	if r.GitHubRepo != "" {
		return []string{r.GitHubRepo}
	}
	return nil
}

// AddRepo adds a repo if not already present. Returns true if added.
func (r *VMRecord) AddRepo(repo string) bool {
	for _, existing := range r.AllGitHubRepos() {
		if existing == repo {
			return false
		}
	}
	r.GitHubRepos = append(r.AllGitHubRepos(), repo)
	return true
}

// RemoveRepo removes a repo. Returns true if removed.
func (r *VMRecord) RemoveRepo(repo string) bool {
	repos := r.AllGitHubRepos()
	for i, existing := range repos {
		if existing == repo {
			r.GitHubRepos = append(repos[:i], repos[i+1:]...)
			return true
		}
	}
	return false
}

// AddProject adds a project if not already present. Returns true if added.
func (r *VMRecord) AddProject(project string) bool {
	for _, existing := range r.GitHubProjects {
		if existing == project {
			return false
		}
	}
	r.GitHubProjects = append(r.GitHubProjects, project)
	return true
}

// RemoveProject removes a project. Returns true if removed.
func (r *VMRecord) RemoveProject(project string) bool {
	for i, existing := range r.GitHubProjects {
		if existing == project {
			r.GitHubProjects = append(r.GitHubProjects[:i], r.GitHubProjects[i+1:]...)
			return true
		}
	}
	return false
}

// SetActivity replaces the VM's latest activity report.
func (r *VMRecord) SetActivity(report *ActivityReport) {
	r.LastActivity = report
}

// SetStatus replaces the VM's latest self-reported Claude Code status.
func (r *VMRecord) SetStatus(status *StatusReport) {
	r.LastStatus = status
}

// SetHealth replaces the VM's latest health report.
func (r *VMRecord) SetHealth(report *HealthReport) {
	r.LastHealth = report
}

// PushMessage appends a message to the VM's inbox.
func (r *VMRecord) PushMessage(msg *Message) {
	r.Inbox = append(r.Inbox, msg)
}

// PopUnread returns all unread messages and removes them from the inbox.
func (r *VMRecord) PopUnread() []*Message {
	var unread []*Message
	var remaining []*Message
	for _, m := range r.Inbox {
		if m.ReadAt == nil {
			unread = append(unread, m)
		} else {
			remaining = append(remaining, m)
		}
	}
	r.Inbox = remaining
	return unread
}

// AckMessages marks messages with the given IDs as read.
func (r *VMRecord) AckMessages(ids []string) {
	now := time.Now()
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}
	for _, m := range r.Inbox {
		if _, ok := idSet[m.ID]; ok && m.ReadAt == nil {
			m.ReadAt = &now
		}
	}
}

// PushMailbox appends a message to the VM's mailbox.
func (r *VMRecord) PushMailbox(msg *MailboxMessage) {
	r.Mailbox = append(r.Mailbox, msg)
}

// ConsumeMailbox returns pending messages and marks them as in-flight.
// Messages whose in-flight timeout has expired are returned to pending first.
func (r *VMRecord) ConsumeMailbox() []*MailboxMessage {
	now := time.Now()
	// Reset expired in-flight messages to pending
	for _, m := range r.Mailbox {
		if m.InFlight != nil && now.Sub(*m.InFlight) > InFlightTimeout {
			m.InFlight = nil
		}
	}
	// Collect and mark pending messages as in-flight
	var result []*MailboxMessage
	for _, m := range r.Mailbox {
		if m.InFlight == nil {
			m.InFlight = &now
			result = append(result, m)
		}
	}
	return result
}

// AckMailbox permanently deletes a message from the mailbox.
func (r *VMRecord) AckMailbox(msgID string) bool {
	for i, m := range r.Mailbox {
		if m.ID == msgID {
			r.Mailbox = append(r.Mailbox[:i], r.Mailbox[i+1:]...)
			return true
		}
	}
	return false
}

// ExportMailbox returns a copy of all mailbox messages (for migration).
func (r *VMRecord) ExportMailbox() []*MailboxMessage {
	if len(r.Mailbox) == 0 {
		return nil
	}
	msgs := make([]*MailboxMessage, len(r.Mailbox))
	for i, m := range r.Mailbox {
		cp := *m
		cp.InFlight = nil // reset in-flight on export
		msgs[i] = &cp
	}
	return msgs
}

// PendingCount returns the number of unread messages.
func (r *VMRecord) PendingCount() int {
	n := 0
	for _, m := range r.Inbox {
		if m.ReadAt == nil {
			n++
		}
	}
	return n
}

type Registry struct {
	mu     sync.RWMutex
	byIP   map[string]*VMRecord
	byID   map[string]*VMRecord
	byMark map[int]*VMRecord
}

func New() *Registry {
	return &Registry{
		byIP:   make(map[string]*VMRecord),
		byID:   make(map[string]*VMRecord),
		byMark: make(map[int]*VMRecord),
	}
}

func (r *Registry) Register(rec *VMRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byIP[rec.IP] = rec
	r.byID[rec.VMID] = rec
	if rec.Mark != 0 {
		r.byMark[rec.Mark] = rec
	}
}

func (r *Registry) Deregister(vmID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	delete(r.byIP, rec.IP)
	delete(r.byID, vmID)
	if rec.Mark != 0 {
		delete(r.byMark, rec.Mark)
	}
	return true
}

func (r *Registry) LookupIP(ip string) (*VMRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byIP[ip]
	return rec, ok
}

func (r *Registry) LookupID(vmID string) (*VMRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byID[vmID]
	return rec, ok
}

func (r *Registry) LookupMark(mark int) (*VMRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byMark[mark]
	return rec, ok
}

func (r *Registry) List() []*VMRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	recs := make([]*VMRecord, 0, len(r.byID))
	for _, rec := range r.byID {
		recs = append(recs, rec)
	}
	return recs
}

// SetActivity updates a VM's activity report under the registry lock.
func (r *Registry) SetActivity(vmID string, report *ActivityReport) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	rec.SetActivity(report)
	return true
}

// SetStatus updates a VM's self-reported Claude Code status under the registry lock.
func (r *Registry) SetStatus(vmID string, status *StatusReport) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	rec.SetStatus(status)
	return true
}

// SetHealth updates a VM's health report under the registry lock.
func (r *Registry) SetHealth(vmID string, report *HealthReport) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	rec.SetHealth(report)
	return true
}

// SetAgentConfig updates a VM's agent configuration under the registry lock.
func (r *Registry) SetAgentConfig(vmID string, cfg *AgentConfig) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	rec.AgentConfig = cfg
	return true
}

// GetAgentConfig returns a VM's agent configuration.
func (r *Registry) GetAgentConfig(vmID string) (*AgentConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return nil, false
	}
	return rec.AgentConfig, true
}

// SetCheckpoint stores a session checkpoint for a VM.
func (r *Registry) SetCheckpoint(vmID string, cp *CheckpointData) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	rec.LastCheckpoint = cp
	return true
}

// GetCheckpoint returns a VM's last session checkpoint.
func (r *Registry) GetCheckpoint(vmID string) (*CheckpointData, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return nil, false
	}
	return rec.LastCheckpoint, true
}

// GetHealth returns a VM's last health report.
func (r *Registry) GetHealth(vmID string) (*HealthReport, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return nil, false
	}
	return rec.LastHealth, true
}

// SetFiles stores a VM's current file tracking data.
func (r *Registry) SetFiles(vmID string, ft *FileTracking) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	rec.LastFiles = ft
	return true
}

// GetFiles returns a VM's file tracking data.
func (r *Registry) GetFiles(vmID string) (*FileTracking, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return nil, false
	}
	return rec.LastFiles, true
}

// PushMessage adds a message to a VM's inbox under the registry lock.
func (r *Registry) PushMessage(vmID string, msg *Message) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	rec.PushMessage(msg)
	return true
}

// PopUnread returns and removes unread messages for a VM under the registry lock.
func (r *Registry) PopUnread(vmID string) ([]*Message, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return nil, false
	}
	return rec.PopUnread(), true
}

// AckMessages marks messages as read for a VM under the registry lock.
func (r *Registry) AckMessages(vmID string, ids []string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	rec.AckMessages(ids)
	return true
}

// PeekInbox returns a VM's inbox without modifying it.
func (r *Registry) PeekInbox(vmID string) ([]*Message, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return nil, false
	}
	msgs := make([]*Message, len(rec.Inbox))
	copy(msgs, rec.Inbox)
	return msgs, true
}

// GetActivity returns a VM's last activity report.
func (r *Registry) GetActivity(vmID string) (*ActivityReport, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return nil, false
	}
	return rec.LastActivity, true
}

// AllActivity returns a summary of tapegun state for all VMs.
func (r *Registry) AllActivity() []VMActivitySummary {
	r.mu.RLock()
	defer r.mu.RUnlock()
	summaries := make([]VMActivitySummary, 0, len(r.byID))
	for _, rec := range r.byID {
		summaries = append(summaries, VMActivitySummary{
			VMID:            rec.VMID,
			LastActivity:    rec.LastActivity,
			LastStatus:      rec.LastStatus,
			LastFiles:       rec.LastFiles,
			LastCheckpoint:  rec.LastCheckpoint,
			AgentConfig:     rec.AgentConfig,
			PendingMessages: rec.PendingCount(),
		})
	}
	return summaries
}

// PushMailbox adds a message to a VM's mailbox under lock.
func (r *Registry) PushMailbox(vmID string, msg *MailboxMessage) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	rec.PushMailbox(msg)
	return true
}

// ConsumeMailbox returns and marks pending messages as in-flight.
func (r *Registry) ConsumeMailbox(vmID string) ([]*MailboxMessage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return nil, false
	}
	return rec.ConsumeMailbox(), true
}

// AckMailbox deletes a message from a VM's mailbox.
func (r *Registry) AckMailbox(vmID string, msgID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	return rec.AckMailbox(msgID)
}

// ExportMailbox returns a copy of a VM's mailbox for migration.
func (r *Registry) ExportMailbox(vmID string) ([]*MailboxMessage, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return nil, false
	}
	return rec.ExportMailbox(), true
}

// ImportMailbox loads messages into a VM's mailbox (after migration).
func (r *Registry) ImportMailbox(vmID string, msgs []*MailboxMessage) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	for _, m := range msgs {
		rec.PushMailbox(m)
	}
	return true
}

// SubscribeChannel returns a channel that receives events for a VM.
// The caller must call the returned cancel function when done.
func (r *Registry) SubscribeChannel(vmID string) (<-chan *ChannelEvent, func(), bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return nil, func() {}, false
	}
	sub := &channelSub{
		ch:   make(chan *ChannelEvent, 32),
		done: make(chan struct{}),
	}
	rec.channelSubs = append(rec.channelSubs, sub)
	cancel := func() {
		close(sub.done)
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, s := range rec.channelSubs {
			if s == sub {
				rec.channelSubs = append(rec.channelSubs[:i], rec.channelSubs[i+1:]...)
				break
			}
		}
	}
	return sub.ch, cancel, true
}

// PushChannelEvent sends an event to all SSE subscribers for a VM.
func (r *Registry) PushChannelEvent(vmID string, evt *ChannelEvent) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	for _, sub := range rec.channelSubs {
		select {
		case sub.ch <- evt:
		case <-sub.done:
		default:
			// subscriber buffer full, drop event
		}
	}
	return true
}

// PostChannelReply stores an outbound reply from an agent.
func (r *Registry) PostChannelReply(vmID string, reply *ChannelReply) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	rec.ChannelReplies = append(rec.ChannelReplies, reply)
	// Keep at most 50 replies
	if len(rec.ChannelReplies) > 50 {
		rec.ChannelReplies = rec.ChannelReplies[len(rec.ChannelReplies)-50:]
	}
	return true
}

// PopChannelReplies returns and removes all pending replies for a VM.
func (r *Registry) PopChannelReplies(vmID string) ([]*ChannelReply, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return nil, false
	}
	replies := rec.ChannelReplies
	rec.ChannelReplies = nil
	return replies, true
}

func (r *Registry) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.List())
}
