package queue

import (
	"database/sql"
	"fmt"
	"time"
)

type WorkItemStatus string

const (
	StatusQueued    WorkItemStatus = "queued"
	StatusAssigned  WorkItemStatus = "assigned"
	StatusCompleted WorkItemStatus = "completed"
	StatusFailed    WorkItemStatus = "failed"
)

// WorkItem represents a unit of work in the queue.
type WorkItem struct {
	ID          int64          `json:"id"`
	Source      string         `json:"source"`                // "github", "manual"
	SourceRef   string         `json:"source_ref"`            // e.g., "owner/repo#42"
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	Priority    int            `json:"priority"`              // higher = more urgent
	Labels      string         `json:"labels"`                // comma-separated
	Team        string         `json:"team,omitempty"`
	Status      WorkItemStatus `json:"status"`
	AssignedTo  string         `json:"assigned_to,omitempty"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	AssignedAt  string         `json:"assigned_at,omitempty"`
	CompletedAt string         `json:"completed_at,omitempty"`
	Attempts    int            `json:"attempts"`
	MaxAttempts int            `json:"max_attempts"`
}

// QueueStats summarises queue state by status.
type QueueStats struct {
	Queued    int `json:"queued"`
	Assigned  int `json:"assigned"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Total     int `json:"total"`
}

// Queue wraps SQLite storage for work items.
type Queue struct {
	db *sql.DB
}

// NewQueue creates a Queue backed by the given SQLite connection.
// The work_queue table must already exist (created by db.migrate).
func NewQueue(db *sql.DB) *Queue {
	return &Queue{db: db}
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// Add inserts a new work item. If an item with the same source_ref already
// exists, it returns (0, nil) — idempotent for GitHub sync.
func (q *Queue) Add(item *WorkItem) (int64, error) {
	ts := now()
	if item.MaxAttempts == 0 {
		item.MaxAttempts = 3
	}
	if item.Status == "" {
		item.Status = StatusQueued
	}
	res, err := q.db.Exec(`INSERT OR IGNORE INTO work_queue
		(source, source_ref, title, body, priority, labels, team, status, created_at, updated_at, max_attempts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.Source, item.SourceRef, item.Title, item.Body,
		item.Priority, item.Labels, item.Team, string(item.Status),
		ts, ts, item.MaxAttempts)
	if err != nil {
		return 0, err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return 0, nil // duplicate source_ref, insert was ignored
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// Get returns a work item by ID.
func (q *Queue) Get(id int64) (*WorkItem, error) {
	row := q.db.QueryRow(`SELECT id, source, source_ref, title, body, priority,
		labels, team, status, assigned_to, created_at, updated_at,
		assigned_at, completed_at, attempts, max_attempts
		FROM work_queue WHERE id=?`, id)
	return scanItem(row)
}

// GetBySourceRef looks up an item by its source reference (e.g., "owner/repo#42").
func (q *Queue) GetBySourceRef(ref string) (*WorkItem, error) {
	row := q.db.QueryRow(`SELECT id, source, source_ref, title, body, priority,
		labels, team, status, assigned_to, created_at, updated_at,
		assigned_at, completed_at, attempts, max_attempts
		FROM work_queue WHERE source_ref=?`, ref)
	item, err := scanItem(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return item, err
}

// ListByStatus returns items with the given status, ordered by priority desc, created_at asc.
func (q *Queue) ListByStatus(status WorkItemStatus) ([]*WorkItem, error) {
	rows, err := q.db.Query(`SELECT id, source, source_ref, title, body, priority,
		labels, team, status, assigned_to, created_at, updated_at,
		assigned_at, completed_at, attempts, max_attempts
		FROM work_queue WHERE status=? ORDER BY priority DESC, created_at ASC`, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// ListAll returns all work items.
func (q *Queue) ListAll() ([]*WorkItem, error) {
	rows, err := q.db.Query(`SELECT id, source, source_ref, title, body, priority,
		labels, team, status, assigned_to, created_at, updated_at,
		assigned_at, completed_at, attempts, max_attempts
		FROM work_queue ORDER BY
			CASE status WHEN 'assigned' THEN 0 WHEN 'queued' THEN 1 WHEN 'failed' THEN 2 ELSE 3 END,
			priority DESC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// Assign marks a queued item as assigned to a VM.
func (q *Queue) Assign(id int64, vmName string) error {
	ts := now()
	res, err := q.db.Exec(`UPDATE work_queue SET status='assigned', assigned_to=?,
		assigned_at=?, updated_at=?, attempts=attempts+1 WHERE id=? AND status='queued'`,
		vmName, ts, ts, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("item %d is not in queued state", id)
	}
	return nil
}

// Complete marks an assigned item as completed.
func (q *Queue) Complete(id int64) error {
	ts := now()
	_, err := q.db.Exec(`UPDATE work_queue SET status='completed', completed_at=?, updated_at=? WHERE id=?`,
		ts, ts, id)
	return err
}

// Fail marks an item as failed.
func (q *Queue) Fail(id int64) error {
	ts := now()
	_, err := q.db.Exec(`UPDATE work_queue SET status='failed', updated_at=? WHERE id=?`, ts, id)
	return err
}

// Reassign returns an assigned item to the queue for re-assignment.
func (q *Queue) Reassign(id int64) error {
	ts := now()
	_, err := q.db.Exec(`UPDATE work_queue SET status='queued', assigned_to='', assigned_at='', updated_at=? WHERE id=?`,
		ts, id)
	return err
}

// StaleAssignments returns items that have been assigned for longer than timeout.
func (q *Queue) StaleAssignments(timeout time.Duration) ([]*WorkItem, error) {
	cutoff := time.Now().UTC().Add(-timeout).Format(time.RFC3339)
	rows, err := q.db.Query(`SELECT id, source, source_ref, title, body, priority,
		labels, team, status, assigned_to, created_at, updated_at,
		assigned_at, completed_at, attempts, max_attempts
		FROM work_queue WHERE status='assigned' AND assigned_at != '' AND assigned_at < ?`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// Stats returns counts by status.
func (q *Queue) Stats() (*QueueStats, error) {
	var s QueueStats
	rows, err := q.db.Query(`SELECT status, COUNT(*) FROM work_queue GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if rows.Scan(&status, &count) == nil {
			switch WorkItemStatus(status) {
			case StatusQueued:
				s.Queued = count
			case StatusAssigned:
				s.Assigned = count
			case StatusCompleted:
				s.Completed = count
			case StatusFailed:
				s.Failed = count
			}
			s.Total += count
		}
	}
	return &s, nil
}

// SyncGitHubIssues adds issues from the GitHub sync to the queue. Returns
// the number of newly added items.
func (q *Queue) SyncGitHubIssues(issues []GitHubIssue, repo string) (int, error) {
	added := 0
	for _, iss := range issues {
		ref := fmt.Sprintf("%s#%d", repo, iss.Number)
		existing, err := q.GetBySourceRef(ref)
		if err != nil {
			return added, err
		}
		if existing != nil {
			continue // already tracked
		}
		priority := issuePriority(iss.Labels)
		_, err = q.Add(&WorkItem{
			Source:    "github",
			SourceRef: ref,
			Title:    iss.Title,
			Body:     iss.Body,
			Priority: priority,
			Labels:   joinLabels(iss.Labels),
		})
		if err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

func issuePriority(labels []GitHubLabel) int {
	for _, l := range labels {
		switch l.Name {
		case "p0-critical":
			return 10
		case "p1-high", "bug":
			return 5
		case "p2-medium":
			return 2
		}
	}
	return 0
}

func joinLabels(labels []GitHubLabel) string {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return fmt.Sprintf("%s", fmt.Sprintf("%s", joinStrings(names)))
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ","
		}
		result += s
	}
	return result
}

// --- helpers ---

type scanner interface {
	Scan(dest ...interface{}) error
}

func scanItem(s scanner) (*WorkItem, error) {
	var w WorkItem
	var status string
	err := s.Scan(&w.ID, &w.Source, &w.SourceRef, &w.Title, &w.Body, &w.Priority,
		&w.Labels, &w.Team, &status, &w.AssignedTo, &w.CreatedAt, &w.UpdatedAt,
		&w.AssignedAt, &w.CompletedAt, &w.Attempts, &w.MaxAttempts)
	if err != nil {
		return nil, err
	}
	w.Status = WorkItemStatus(status)
	return &w, nil
}

func scanItems(rows *sql.Rows) ([]*WorkItem, error) {
	var items []*WorkItem
	for rows.Next() {
		w, err := scanItem(rows)
		if err != nil {
			return items, err
		}
		items = append(items, w)
	}
	return items, rows.Err()
}
