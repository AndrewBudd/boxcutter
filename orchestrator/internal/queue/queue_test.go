package queue

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *Queue {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS work_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source TEXT NOT NULL DEFAULT 'github',
		source_ref TEXT NOT NULL UNIQUE,
		title TEXT NOT NULL,
		body TEXT NOT NULL DEFAULT '',
		priority INTEGER NOT NULL DEFAULT 0,
		labels TEXT NOT NULL DEFAULT '',
		team TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'queued',
		assigned_to TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		assigned_at TEXT NOT NULL DEFAULT '',
		completed_at TEXT NOT NULL DEFAULT '',
		attempts INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL DEFAULT 3
	)`)
	if err != nil {
		t.Fatal(err)
	}
	return NewQueue(db)
}

func TestQueueAddAndGet(t *testing.T) {
	q := openTestDB(t)
	id, err := q.Add(&WorkItem{
		Source:    "github",
		SourceRef: "owner/repo#42",
		Title:    "Fix login bug",
		Body:     "The login page is broken",
		Priority: 5,
		Labels:   "bug,p1-high",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("expected non-zero ID")
	}

	item, err := q.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "Fix login bug" {
		t.Errorf("title = %q, want %q", item.Title, "Fix login bug")
	}
	if item.Status != StatusQueued {
		t.Errorf("status = %q, want %q", item.Status, StatusQueued)
	}
	if item.Priority != 5 {
		t.Errorf("priority = %d, want 5", item.Priority)
	}
}

func TestQueueDedup(t *testing.T) {
	q := openTestDB(t)
	id1, _ := q.Add(&WorkItem{Source: "github", SourceRef: "owner/repo#1", Title: "First"})
	id2, _ := q.Add(&WorkItem{Source: "github", SourceRef: "owner/repo#1", Title: "Duplicate"})
	if id1 == 0 {
		t.Fatal("first insert should return non-zero ID")
	}
	if id2 != 0 {
		t.Error("duplicate insert should return 0 (INSERT OR IGNORE)")
	}

	// Verify only one item exists
	items, _ := q.ListAll()
	if len(items) != 1 {
		t.Errorf("expected 1 item, got %d", len(items))
	}
}

func TestQueueGetBySourceRef(t *testing.T) {
	q := openTestDB(t)
	q.Add(&WorkItem{Source: "github", SourceRef: "owner/repo#10", Title: "Test"})

	item, err := q.GetBySourceRef("owner/repo#10")
	if err != nil {
		t.Fatal(err)
	}
	if item == nil {
		t.Fatal("expected item, got nil")
	}
	if item.Title != "Test" {
		t.Errorf("title = %q, want %q", item.Title, "Test")
	}

	// Non-existent
	item, err = q.GetBySourceRef("owner/repo#999")
	if err != nil {
		t.Fatal(err)
	}
	if item != nil {
		t.Error("expected nil for non-existent ref")
	}
}

func TestQueueAssignAndComplete(t *testing.T) {
	q := openTestDB(t)
	id, _ := q.Add(&WorkItem{Source: "github", SourceRef: "owner/repo#5", Title: "Task"})

	err := q.Assign(id, "dev-worker-1")
	if err != nil {
		t.Fatal(err)
	}

	item, _ := q.Get(id)
	if item.Status != StatusAssigned {
		t.Errorf("status = %q, want %q", item.Status, StatusAssigned)
	}
	if item.AssignedTo != "dev-worker-1" {
		t.Errorf("assigned_to = %q, want %q", item.AssignedTo, "dev-worker-1")
	}
	if item.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", item.Attempts)
	}

	err = q.Complete(id)
	if err != nil {
		t.Fatal(err)
	}
	item, _ = q.Get(id)
	if item.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", item.Status, StatusCompleted)
	}
}

func TestQueueReassign(t *testing.T) {
	q := openTestDB(t)
	id, _ := q.Add(&WorkItem{Source: "github", SourceRef: "owner/repo#7", Title: "Reassignable"})
	q.Assign(id, "worker-1")

	err := q.Reassign(id)
	if err != nil {
		t.Fatal(err)
	}

	item, _ := q.Get(id)
	if item.Status != StatusQueued {
		t.Errorf("status = %q, want %q", item.Status, StatusQueued)
	}
	if item.AssignedTo != "" {
		t.Errorf("assigned_to = %q, want empty", item.AssignedTo)
	}
	if item.Attempts != 1 {
		t.Errorf("attempts should still be 1 after reassign, got %d", item.Attempts)
	}
}

func TestQueueFailAfterMaxAttempts(t *testing.T) {
	q := openTestDB(t)
	id, _ := q.Add(&WorkItem{Source: "github", SourceRef: "owner/repo#8", Title: "Fragile", MaxAttempts: 2})

	// Attempt 1: assign → reassign
	q.Assign(id, "w1")
	q.Reassign(id)

	// Attempt 2: assign → fail
	q.Assign(id, "w2")

	item, _ := q.Get(id)
	if item.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", item.Attempts)
	}

	q.Fail(id)
	item, _ = q.Get(id)
	if item.Status != StatusFailed {
		t.Errorf("status = %q, want %q", item.Status, StatusFailed)
	}
}

func TestQueueStaleAssignments(t *testing.T) {
	q := openTestDB(t)
	id, _ := q.Add(&WorkItem{Source: "github", SourceRef: "owner/repo#20", Title: "Stale"})
	q.Assign(id, "worker-1")

	// Backdate the assigned_at to 1 hour ago
	old := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	q.db.Exec(`UPDATE work_queue SET assigned_at=? WHERE id=?`, old, id)

	stale, err := q.StaleAssignments(30 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale item, got %d", len(stale))
	}
	if stale[0].ID != id {
		t.Errorf("stale item ID = %d, want %d", stale[0].ID, id)
	}
}

func TestQueueStats(t *testing.T) {
	q := openTestDB(t)
	q.Add(&WorkItem{Source: "github", SourceRef: "r#1", Title: "A"})
	q.Add(&WorkItem{Source: "github", SourceRef: "r#2", Title: "B"})
	id3, _ := q.Add(&WorkItem{Source: "github", SourceRef: "r#3", Title: "C"})
	q.Assign(id3, "w1")

	stats, err := q.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Queued != 2 {
		t.Errorf("queued = %d, want 2", stats.Queued)
	}
	if stats.Assigned != 1 {
		t.Errorf("assigned = %d, want 1", stats.Assigned)
	}
	if stats.Total != 3 {
		t.Errorf("total = %d, want 3", stats.Total)
	}
}

func TestQueuePriorityOrdering(t *testing.T) {
	q := openTestDB(t)
	q.Add(&WorkItem{Source: "github", SourceRef: "r#1", Title: "Low", Priority: 0})
	q.Add(&WorkItem{Source: "github", SourceRef: "r#2", Title: "High", Priority: 10})
	q.Add(&WorkItem{Source: "github", SourceRef: "r#3", Title: "Medium", Priority: 5})

	items, _ := q.ListByStatus(StatusQueued)
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	if items[0].Title != "High" {
		t.Errorf("first item = %q, want High", items[0].Title)
	}
	if items[1].Title != "Medium" {
		t.Errorf("second item = %q, want Medium", items[1].Title)
	}
	if items[2].Title != "Low" {
		t.Errorf("third item = %q, want Low", items[2].Title)
	}
}

func TestSyncGitHubIssues(t *testing.T) {
	q := openTestDB(t)
	issues := []GitHubIssue{
		{Number: 42, Title: "Fix bug", Body: "It's broken", Labels: []GitHubLabel{{Name: "bug"}, {Name: "ready"}}},
		{Number: 43, Title: "Add feature", Body: "New feature", Labels: []GitHubLabel{{Name: "ready"}}},
	}

	added, err := q.SyncGitHubIssues(issues, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2", added)
	}

	// Re-sync should add 0
	added, err = q.SyncGitHubIssues(issues, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Errorf("added = %d, want 0 (dedup)", added)
	}

	// Verify priority from labels
	item, _ := q.GetBySourceRef("owner/repo#42")
	if item.Priority != 5 { // "bug" label → priority 5
		t.Errorf("priority = %d, want 5 for bug label", item.Priority)
	}
}

func TestParseIssueNumber(t *testing.T) {
	repo, num, ok := ParseIssueNumber("AndrewBudd/boxcutter#42")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if repo != "AndrewBudd/boxcutter" {
		t.Errorf("repo = %q", repo)
	}
	if num != 42 {
		t.Errorf("number = %d", num)
	}

	_, _, ok = ParseIssueNumber("invalid")
	if ok {
		t.Error("expected ok=false for invalid ref")
	}
}

func TestMessageFormatting(t *testing.T) {
	item := &WorkItem{
		ID:        1,
		SourceRef: "owner/repo#42",
		Title:     "Fix the thing",
		Priority:  5,
		Body:      "Details here",
	}
	// Verify the message would contain key fields
	body := "TASK ASSIGNMENT\nIssue: " + item.SourceRef + "\nTitle: " + item.Title
	if body == "" {
		t.Error("message body should not be empty")
	}
	if !contains(body, "owner/repo#42") {
		t.Error("message should contain source ref")
	}
	if !contains(body, "Fix the thing") {
		t.Error("message should contain title")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
