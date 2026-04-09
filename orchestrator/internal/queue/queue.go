// Package queue implements a work queue that syncs GitHub issues and assigns
// them to idle workers via tapegun inbox messages.
package queue

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// WorkItem is a unit of work in the queue, typically derived from a GitHub issue.
type WorkItem struct {
	ID          string     `json:"id"`
	Source      string     `json:"source"`       // "github-issue", "manual"
	SourceRef   string     `json:"source_ref"`   // e.g. "AndrewBudd/boxcutter#42"
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Priority    int        `json:"priority"`     // 0=highest
	Labels      []string   `json:"labels"`
	Team        string     `json:"team"`
	Status      string     `json:"status"`       // "queued", "assigned", "completed", "failed"
	AssignedTo  string     `json:"assigned_to"`
	AssignedAt  *time.Time `json:"assigned_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Attempts    int        `json:"attempts"`
	MaxAttempts int        `json:"max_attempts"`
}

// QueueConfig controls how the queue syncs and assigns work.
type QueueConfig struct {
	Repo           string            // GitHub repo (owner/repo)
	Label          string            // issue label to sync (e.g. "ready")
	PollInterval   time.Duration     // how often to sync issues
	AssignInterval time.Duration     // how often to check for idle workers
	TimeoutMinutes int               // assignment timeout in minutes
	PriorityLabels map[string]int    // label -> priority mapping
}

// Queue manages work items backed by SQLite.
type Queue struct {
	db     *sql.DB
	cfg    QueueConfig
	mu     sync.RWMutex
	stopCh chan struct{}
}

// New creates a Queue backed by the given SQLite connection.
// Call Migrate() before use.
func New(conn *sql.DB, cfg QueueConfig) *Queue {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 60 * time.Second
	}
	if cfg.AssignInterval == 0 {
		cfg.AssignInterval = 15 * time.Second
	}
	if cfg.TimeoutMinutes == 0 {
		cfg.TimeoutMinutes = 30
	}
	return &Queue{db: conn, cfg: cfg, stopCh: make(chan struct{})}
}

// Migrate creates the work_queue table if it doesn't exist.
func (q *Queue) Migrate() error {
	_, err := q.db.Exec(`
	CREATE TABLE IF NOT EXISTS work_queue (
		id          TEXT PRIMARY KEY,
		source      TEXT NOT NULL DEFAULT 'manual',
		source_ref  TEXT NOT NULL DEFAULT '',
		title       TEXT NOT NULL,
		body        TEXT NOT NULL DEFAULT '',
		priority    INTEGER NOT NULL DEFAULT 2,
		labels      TEXT NOT NULL DEFAULT '[]',
		team        TEXT NOT NULL DEFAULT '',
		status      TEXT NOT NULL DEFAULT 'queued',
		assigned_to TEXT NOT NULL DEFAULT '',
		assigned_at TEXT,
		completed_at TEXT,
		created_at  TEXT NOT NULL,
		updated_at  TEXT NOT NULL,
		attempts    INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL DEFAULT 2
	)`)
	return err
}

// Enqueue adds a work item to the queue. If an item with the same source_ref
// already exists and is not completed/failed, it is skipped.
func (q *Queue) Enqueue(item *WorkItem) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if item.SourceRef != "" {
		var existing string
		err := q.db.QueryRow(
			`SELECT status FROM work_queue WHERE source_ref = ? AND status NOT IN ('completed', 'failed')`,
			item.SourceRef).Scan(&existing)
		if err == nil {
			return nil // already queued or assigned
		}
	}

	if item.ID == "" {
		item.ID = fmt.Sprintf("wq-%d", time.Now().UnixNano())
	}
	if item.MaxAttempts == 0 {
		item.MaxAttempts = 2
	}
	now := time.Now()
	item.CreatedAt = now
	item.UpdatedAt = now
	item.Status = "queued"

	labelsJSON, _ := json.Marshal(item.Labels)

	_, err := q.db.Exec(`INSERT INTO work_queue
		(id, source, source_ref, title, body, priority, labels, team, status, assigned_to, created_at, updated_at, max_attempts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.Source, item.SourceRef, item.Title, item.Body,
		item.Priority, string(labelsJSON), item.Team, item.Status, item.AssignedTo,
		now.Format(time.RFC3339), now.Format(time.RFC3339), item.MaxAttempts)
	if err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	log.Printf("queue: enqueued %s — %s (priority %d)", item.ID, item.Title, item.Priority)
	return nil
}

// NextQueued returns the highest-priority unassigned item for a team (or any team if empty).
func (q *Queue) NextQueued(team string) *WorkItem {
	q.mu.RLock()
	defer q.mu.RUnlock()

	query := `SELECT id, source, source_ref, title, body, priority, labels, team, status,
		assigned_to, assigned_at, completed_at, created_at, updated_at, attempts, max_attempts
		FROM work_queue WHERE status = 'queued'`
	args := []interface{}{}
	if team != "" {
		query += ` AND (team = ? OR team = '')`
		args = append(args, team)
	}
	query += ` ORDER BY priority ASC, created_at ASC LIMIT 1`

	return q.scanItem(q.db.QueryRow(query, args...))
}

// Assign marks a queued item as assigned to the given VM.
func (q *Queue) Assign(itemID, vmName string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	res, err := q.db.Exec(`UPDATE work_queue SET status='assigned', assigned_to=?, assigned_at=?, updated_at=?, attempts=attempts+1
		WHERE id=? AND status='queued'`,
		vmName, now.Format(time.RFC3339), now.Format(time.RFC3339), itemID)
	if err != nil {
		return fmt.Errorf("assign: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("item %s not in queued state", itemID)
	}
	log.Printf("queue: assigned %s to %s", itemID, vmName)
	return nil
}

// Complete marks an item as completed.
func (q *Queue) Complete(itemID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	_, err := q.db.Exec(`UPDATE work_queue SET status='completed', completed_at=?, updated_at=? WHERE id=?`,
		now.Format(time.RFC3339), now.Format(time.RFC3339), itemID)
	if err != nil {
		return fmt.Errorf("complete: %w", err)
	}
	log.Printf("queue: completed %s", itemID)
	return nil
}

// Fail marks an item as failed. If attempts < max_attempts, it is re-enqueued.
func (q *Queue) Fail(itemID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	var attempts, maxAttempts int
	err := q.db.QueryRow(`SELECT attempts, max_attempts FROM work_queue WHERE id=?`, itemID).Scan(&attempts, &maxAttempts)
	if err != nil {
		return fmt.Errorf("fail lookup: %w", err)
	}

	now := time.Now()
	if attempts < maxAttempts {
		_, err = q.db.Exec(`UPDATE work_queue SET status='queued', assigned_to='', assigned_at=NULL, updated_at=? WHERE id=?`,
			now.Format(time.RFC3339), itemID)
		log.Printf("queue: re-enqueued %s (attempt %d/%d)", itemID, attempts, maxAttempts)
	} else {
		_, err = q.db.Exec(`UPDATE work_queue SET status='failed', updated_at=? WHERE id=?`,
			now.Format(time.RFC3339), itemID)
		log.Printf("queue: failed %s (max attempts reached)", itemID)
	}
	return err
}

// Reassign forces a re-queue of an assigned item so it can be picked up by another worker.
func (q *Queue) Reassign(itemID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	res, err := q.db.Exec(`UPDATE work_queue SET status='queued', assigned_to='', assigned_at=NULL, updated_at=? WHERE id=? AND status='assigned'`,
		now.Format(time.RFC3339), itemID)
	if err != nil {
		return fmt.Errorf("reassign: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("item %s not in assigned state", itemID)
	}
	log.Printf("queue: reassigned %s back to queue", itemID)
	return nil
}

// TimeoutStale re-enqueues items that have been assigned for longer than the timeout.
func (q *Queue) TimeoutStale() []WorkItem {
	q.mu.Lock()
	defer q.mu.Unlock()

	cutoff := time.Now().Add(-time.Duration(q.cfg.TimeoutMinutes) * time.Minute).Format(time.RFC3339)
	now := time.Now().Format(time.RFC3339)

	rows, err := q.db.Query(`SELECT id, title, assigned_to FROM work_queue WHERE status='assigned' AND assigned_at < ?`, cutoff)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var timedOut []WorkItem
	for rows.Next() {
		var id, title, assignedTo string
		if rows.Scan(&id, &title, &assignedTo) == nil {
			timedOut = append(timedOut, WorkItem{ID: id, Title: title, AssignedTo: assignedTo})
		}
	}

	for _, item := range timedOut {
		q.db.Exec(`UPDATE work_queue SET status='queued', assigned_to='', assigned_at=NULL, updated_at=? WHERE id=?`,
			now, item.ID)
		log.Printf("queue: timed out %s (was assigned to %s)", item.ID, item.AssignedTo)
	}

	return timedOut
}

// List returns all items matching the given status filter (empty = all).
func (q *Queue) List(statusFilter string) []*WorkItem {
	q.mu.RLock()
	defer q.mu.RUnlock()

	query := `SELECT id, source, source_ref, title, body, priority, labels, team, status,
		assigned_to, assigned_at, completed_at, created_at, updated_at, attempts, max_attempts
		FROM work_queue`
	args := []interface{}{}
	if statusFilter != "" {
		query += ` WHERE status = ?`
		args = append(args, statusFilter)
	}
	query += ` ORDER BY priority ASC, created_at ASC`

	rows, err := q.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var items []*WorkItem
	for rows.Next() {
		item := q.scanItemRow(rows)
		if item != nil {
			items = append(items, item)
		}
	}
	return items
}

// Get returns a single work item by ID.
func (q *Queue) Get(id string) *WorkItem {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.scanItem(q.db.QueryRow(`SELECT id, source, source_ref, title, body, priority, labels, team, status,
		assigned_to, assigned_at, completed_at, created_at, updated_at, attempts, max_attempts
		FROM work_queue WHERE id = ?`, id))
}

// GetBySourceRef finds an item by its source reference (e.g. "owner/repo#42").
func (q *Queue) GetBySourceRef(ref string) *WorkItem {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.scanItem(q.db.QueryRow(`SELECT id, source, source_ref, title, body, priority, labels, team, status,
		assigned_to, assigned_at, completed_at, created_at, updated_at, attempts, max_attempts
		FROM work_queue WHERE source_ref = ? ORDER BY created_at DESC LIMIT 1`, ref))
}

// AssignedTo returns the currently assigned item for a VM, if any.
func (q *Queue) AssignedTo(vmName string) *WorkItem {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.scanItem(q.db.QueryRow(`SELECT id, source, source_ref, title, body, priority, labels, team, status,
		assigned_to, assigned_at, completed_at, created_at, updated_at, attempts, max_attempts
		FROM work_queue WHERE assigned_to = ? AND status = 'assigned' LIMIT 1`, vmName))
}

func (q *Queue) scanItem(row *sql.Row) *WorkItem {
	var item WorkItem
	var labelsJSON, assignedAt, completedAt, createdAt, updatedAt string
	var assignedAtPtr, completedAtPtr *string

	err := row.Scan(&item.ID, &item.Source, &item.SourceRef, &item.Title, &item.Body,
		&item.Priority, &labelsJSON, &item.Team, &item.Status,
		&item.AssignedTo, &assignedAtPtr, &completedAtPtr, &createdAt, &updatedAt,
		&item.Attempts, &item.MaxAttempts)
	if err != nil {
		return nil
	}

	json.Unmarshal([]byte(labelsJSON), &item.Labels)
	item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	item.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	if assignedAtPtr != nil {
		assignedAt = *assignedAtPtr
		t, _ := time.Parse(time.RFC3339, assignedAt)
		item.AssignedAt = &t
	}
	if completedAtPtr != nil {
		completedAt = *completedAtPtr
		t, _ := time.Parse(time.RFC3339, completedAt)
		item.CompletedAt = &t
	}
	return &item
}

func (q *Queue) scanItemRow(rows *sql.Rows) *WorkItem {
	var item WorkItem
	var labelsJSON, createdAt, updatedAt string
	var assignedAtPtr, completedAtPtr *string

	err := rows.Scan(&item.ID, &item.Source, &item.SourceRef, &item.Title, &item.Body,
		&item.Priority, &labelsJSON, &item.Team, &item.Status,
		&item.AssignedTo, &assignedAtPtr, &completedAtPtr, &createdAt, &updatedAt,
		&item.Attempts, &item.MaxAttempts)
	if err != nil {
		return nil
	}

	json.Unmarshal([]byte(labelsJSON), &item.Labels)
	item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	item.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	if assignedAtPtr != nil {
		t, _ := time.Parse(time.RFC3339, *assignedAtPtr)
		item.AssignedAt = &t
	}
	if completedAtPtr != nil {
		t, _ := time.Parse(time.RFC3339, *completedAtPtr)
		item.CompletedAt = &t
	}
	return &item
}

// PriorityFromLabels returns the highest priority (lowest number) from issue labels.
func PriorityFromLabels(issueLabels []string, mapping map[string]int) int {
	best := 2 // default medium priority
	for _, l := range issueLabels {
		if p, ok := mapping[l]; ok && p < best {
			best = p
		}
	}
	return best
}

// FormatAssignmentMessage builds the tapegun inbox message body for an assignment.
func FormatAssignmentMessage(item *WorkItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TASK ASSIGNMENT\n")
	fmt.Fprintf(&b, "Issue: %s\n", item.SourceRef)
	fmt.Fprintf(&b, "Title: %s\n", item.Title)
	fmt.Fprintf(&b, "Priority: %s\n", PriorityName(item.Priority))
	if item.Body != "" {
		// Truncate body to avoid overwhelming the inbox
		body := item.Body
		if len(body) > 2000 {
			body = body[:2000] + "\n...(truncated)"
		}
		fmt.Fprintf(&b, "\n%s\n", body)
	}
	fmt.Fprintf(&b, "\nPlease work on this issue. Read the full issue on GitHub for context. When done, create a PR that references the issue.")
	return b.String()
}

// PriorityName returns a human-readable priority name.
func PriorityName(p int) string {
	switch {
	case p == 0:
		return "p0-critical"
	case p == 1:
		return "p1-high"
	case p == 2:
		return "p2-medium"
	default:
		return "p3-low"
	}
}
