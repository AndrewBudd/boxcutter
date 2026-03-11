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

// Message is a directive sent to a VM by an external wingman agent.
type Message struct {
	ID        string     `json:"id"`
	From      string     `json:"from"`
	Body      string     `json:"body"`
	Priority  string     `json:"priority"`
	SendKeys  bool       `json:"send_keys,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
}

// VMActivitySummary is a lightweight view of a VM's wingman state.
type VMActivitySummary struct {
	VMID            string          `json:"vm_id"`
	LastActivity    *ActivityReport `json:"last_activity,omitempty"`
	PendingMessages int             `json:"pending_messages"`
}

type VMRecord struct {
	VMID        string            `json:"vm_id"`
	IP          string            `json:"ip"`
	Mark        int               `json:"mark"`
	Mode        string            `json:"mode"`
	Labels      map[string]string `json:"labels,omitempty"`
	GitHubRepo  string            `json:"github_repo,omitempty"`  // backwards compat
	GitHubRepos []string          `json:"github_repos,omitempty"` // all repos

	LastActivity *ActivityReport `json:"last_activity,omitempty"`
	Inbox        []*Message      `json:"inbox,omitempty"`
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

// SetActivity replaces the VM's latest activity report.
func (r *VMRecord) SetActivity(report *ActivityReport) {
	r.LastActivity = report
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

// AllActivity returns a summary of wingman state for all VMs.
func (r *Registry) AllActivity() []VMActivitySummary {
	r.mu.RLock()
	defer r.mu.RUnlock()
	summaries := make([]VMActivitySummary, 0, len(r.byID))
	for _, rec := range r.byID {
		summaries = append(summaries, VMActivitySummary{
			VMID:            rec.VMID,
			LastActivity:    rec.LastActivity,
			PendingMessages: rec.PendingCount(),
		})
	}
	return summaries
}

func (r *Registry) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.List())
}
