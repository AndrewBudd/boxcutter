package queue

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("PRAGMA journal_mode=WAL")
	return db
}

func setupTestQueue(t *testing.T) *Queue {
	t.Helper()
	db := setupTestDB(t)
	q := New(db, QueueConfig{
		TimeoutMinutes: 30,
		PriorityLabels: map[string]int{
			"p0-critical": 0,
			"p1-high":     1,
			"bug":         1,
			"p2-medium":   2,
		},
	})
	if err := q.Migrate(); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestEnqueueAndList(t *testing.T) {
	q := setupTestQueue(t)

	err := q.Enqueue(&WorkItem{
		Source:    "manual",
		Title:    "Fix login bug",
		Priority: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = q.Enqueue(&WorkItem{
		Source:    "github-issue",
		SourceRef: "owner/repo#42",
		Title:    "Add retry logic",
		Priority: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	items := q.List("")
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	// Should be sorted by priority (lower = higher priority)
	if items[0].Priority != 1 {
		t.Errorf("expected first item priority 1, got %d", items[0].Priority)
	}
	if items[1].Priority != 2 {
		t.Errorf("expected second item priority 2, got %d", items[1].Priority)
	}
}

func TestEnqueueDedup(t *testing.T) {
	q := setupTestQueue(t)

	q.Enqueue(&WorkItem{
		Source:    "github-issue",
		SourceRef: "owner/repo#42",
		Title:    "Fix bug",
	})
	q.Enqueue(&WorkItem{
		Source:    "github-issue",
		SourceRef: "owner/repo#42",
		Title:    "Fix bug (duplicate)",
	})

	items := q.List("")
	if len(items) != 1 {
		t.Fatalf("expected dedup to 1 item, got %d", len(items))
	}
}

func TestAssignAndComplete(t *testing.T) {
	q := setupTestQueue(t)

	q.Enqueue(&WorkItem{
		Source: "manual",
		Title:  "Task 1",
	})

	item := q.NextQueued("")
	if item == nil {
		t.Fatal("expected a queued item")
	}

	err := q.Assign(item.ID, "dev-worker-1")
	if err != nil {
		t.Fatal(err)
	}

	// Should not appear as next queued
	next := q.NextQueued("")
	if next != nil {
		t.Error("expected no more queued items")
	}

	// Should be assigned to the worker
	assigned := q.AssignedTo("dev-worker-1")
	if assigned == nil {
		t.Fatal("expected item assigned to dev-worker-1")
	}
	if assigned.ID != item.ID {
		t.Errorf("assigned item ID mismatch: %s != %s", assigned.ID, item.ID)
	}

	err = q.Complete(item.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Should no longer be assigned
	assigned = q.AssignedTo("dev-worker-1")
	if assigned != nil {
		t.Error("expected no assigned item after completion")
	}

	// List completed
	completed := q.List("completed")
	if len(completed) != 1 {
		t.Fatalf("expected 1 completed, got %d", len(completed))
	}
}

func TestFailAndReenqueue(t *testing.T) {
	q := setupTestQueue(t)

	q.Enqueue(&WorkItem{
		Source:      "manual",
		Title:       "Retry task",
		MaxAttempts: 2,
	})

	item := q.NextQueued("")
	q.Assign(item.ID, "worker-1")
	q.Fail(item.ID)

	// Should be re-enqueued (attempt 1 < max 2)
	requeued := q.NextQueued("")
	if requeued == nil {
		t.Fatal("expected item to be re-enqueued")
	}
	if requeued.Attempts != 1 {
		t.Errorf("expected 1 attempt, got %d", requeued.Attempts)
	}

	// Assign and fail again
	q.Assign(requeued.ID, "worker-2")
	q.Fail(requeued.ID)

	// Should now be failed (attempt 2 >= max 2)
	failed := q.List("failed")
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed item, got %d", len(failed))
	}
}

func TestReassign(t *testing.T) {
	q := setupTestQueue(t)

	q.Enqueue(&WorkItem{
		Source: "manual",
		Title:  "Reassign me",
	})

	item := q.NextQueued("")
	q.Assign(item.ID, "worker-1")

	err := q.Reassign(item.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Should be back in the queue
	next := q.NextQueued("")
	if next == nil {
		t.Fatal("expected item back in queue after reassign")
	}
	if next.AssignedTo != "" {
		t.Error("expected assigned_to to be cleared")
	}
}

func TestTimeoutStale(t *testing.T) {
	q := setupTestQueue(t)

	q.Enqueue(&WorkItem{
		Source: "manual",
		Title:  "Stale task",
	})

	item := q.NextQueued("")
	q.Assign(item.ID, "slow-worker")

	// Backdate the assigned_at to make it stale
	q.mu.Lock()
	past := time.Now().Add(-31 * time.Minute).Format(time.RFC3339)
	q.db.Exec(`UPDATE work_queue SET assigned_at = ? WHERE id = ?`, past, item.ID)
	q.mu.Unlock()

	timedOut := q.TimeoutStale()
	if len(timedOut) != 1 {
		t.Fatalf("expected 1 timed out, got %d", len(timedOut))
	}

	// Should be back in queue
	next := q.NextQueued("")
	if next == nil {
		t.Fatal("expected stale item re-enqueued")
	}
}

func TestPriorityOrdering(t *testing.T) {
	q := setupTestQueue(t)

	q.Enqueue(&WorkItem{Source: "manual", Title: "Low", Priority: 3})
	q.Enqueue(&WorkItem{Source: "manual", Title: "Critical", Priority: 0})
	q.Enqueue(&WorkItem{Source: "manual", Title: "High", Priority: 1})

	// NextQueued should return highest priority (lowest number) first
	item := q.NextQueued("")
	if item == nil || item.Title != "Critical" {
		t.Fatalf("expected Critical first, got %v", item)
	}
}

func TestPriorityFromLabels(t *testing.T) {
	mapping := map[string]int{
		"p0-critical": 0,
		"p1-high":     1,
		"bug":         1,
		"p2-medium":   2,
	}

	tests := []struct {
		labels   []string
		expected int
	}{
		{[]string{"bug", "p2-medium"}, 1},
		{[]string{"p0-critical"}, 0},
		{[]string{"enhancement"}, 2}, // default
		{[]string{}, 2},
	}

	for _, tt := range tests {
		got := PriorityFromLabels(tt.labels, mapping)
		if got != tt.expected {
			t.Errorf("PriorityFromLabels(%v) = %d, want %d", tt.labels, got, tt.expected)
		}
	}
}

func TestFormatAssignmentMessage(t *testing.T) {
	item := &WorkItem{
		SourceRef: "owner/repo#42",
		Title:     "Fix login timeout",
		Body:      "The login page times out after 30 seconds.",
		Priority:  1,
	}

	msg := FormatAssignmentMessage(item)
	if msg == "" {
		t.Fatal("expected non-empty message")
	}

	// Should contain key fields
	for _, want := range []string{"TASK ASSIGNMENT", "owner/repo#42", "Fix login timeout", "p1-high"} {
		if !containsStr(msg, want) {
			t.Errorf("message missing %q", want)
		}
	}
}

func TestParseIssueRef(t *testing.T) {
	repo, num, ok := ParseIssueRef("AndrewBudd/boxcutter#42")
	if !ok || repo != "AndrewBudd/boxcutter" || num != 42 {
		t.Errorf("unexpected result: %s, %d, %v", repo, num, ok)
	}

	_, _, ok = ParseIssueRef("invalid")
	if ok {
		t.Error("expected invalid ref to fail")
	}
}

func TestGetBySourceRef(t *testing.T) {
	q := setupTestQueue(t)

	q.Enqueue(&WorkItem{
		Source:    "github-issue",
		SourceRef: "owner/repo#99",
		Title:    "Test issue",
	})

	item := q.GetBySourceRef("owner/repo#99")
	if item == nil {
		t.Fatal("expected to find item by source ref")
	}
	if item.Title != "Test issue" {
		t.Errorf("expected title 'Test issue', got %q", item.Title)
	}

	missing := q.GetBySourceRef("owner/repo#999")
	if missing != nil {
		t.Error("expected nil for missing ref")
	}
}

func TestListByStatus(t *testing.T) {
	q := setupTestQueue(t)

	q.Enqueue(&WorkItem{Source: "manual", Title: "Queued 1"})
	q.Enqueue(&WorkItem{Source: "manual", Title: "Queued 2"})

	item := q.NextQueued("")
	q.Assign(item.ID, "worker")

	queued := q.List("queued")
	if len(queued) != 1 {
		t.Errorf("expected 1 queued, got %d", len(queued))
	}

	assigned := q.List("assigned")
	if len(assigned) != 1 {
		t.Errorf("expected 1 assigned, got %d", len(assigned))
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStrHelper(s, substr))
}

func containsStrHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
