package collab

import (
	"testing"
)

func TestSendAndReceiveMessage(t *testing.T) {
	hub := NewHub()

	hub.SendMessage(&TeamMessage{
		From: "eng-manager",
		To:   "dev-worker-1",
		Team: "platform",
		Type: "notification",
		Body: "PR approved, merge when CI passes",
	})

	msgs := hub.PendingMessages("platform", "dev-worker-1")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Body != "PR approved, merge when CI passes" {
		t.Errorf("unexpected body: %s", msgs[0].Body)
	}

	// Should be consumed
	msgs2 := hub.PendingMessages("platform", "dev-worker-1")
	if len(msgs2) != 0 {
		t.Fatalf("expected 0 messages after consume, got %d", len(msgs2))
	}
}

func TestBroadcastMessage(t *testing.T) {
	hub := NewHub()

	// Broadcast (To="") should be delivered to any agent
	hub.SendMessage(&TeamMessage{
		From: "eng-manager",
		Team: "platform",
		Type: "decision",
		Body: "Use jwt-go v5",
	})

	// Any agent on the team should get it
	msgs := hub.PendingMessages("platform", "dev-worker-1")
	if len(msgs) != 1 {
		t.Fatalf("expected broadcast message, got %d", len(msgs))
	}
}

func TestTargetedMessageNotDeliveredToOthers(t *testing.T) {
	hub := NewHub()

	hub.SendMessage(&TeamMessage{
		From: "eng-manager",
		To:   "dev-worker-1",
		Team: "platform",
		Type: "request",
		Body: "Review PR #42",
	})

	// Different agent shouldn't get it
	msgs := hub.PendingMessages("platform", "dev-worker-2")
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages for wrong agent, got %d", len(msgs))
	}

	// Correct agent should
	msgs = hub.PendingMessages("platform", "dev-worker-1")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message for correct agent, got %d", len(msgs))
	}
}

func TestConflictDetection(t *testing.T) {
	hub := NewHub()

	hub.ReportFiles(&FileReport{
		Agent:  "dev-worker-1",
		Team:   "platform",
		Branch: "fix/auth",
		Files:  []string{"api/handlers.go", "api/middleware.go"},
	})

	hub.ReportFiles(&FileReport{
		Agent:  "dev-worker-2",
		Team:   "platform",
		Branch: "feat/rate-limit",
		Files:  []string{"api/handlers.go", "api/rate_limit.go"},
	})

	conflicts := hub.DetectConflicts("platform")
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
	if conflicts[0].File != "api/handlers.go" {
		t.Errorf("expected conflict on api/handlers.go, got %s", conflicts[0].File)
	}
	if conflicts[0].AgentA != "dev-worker-1" || conflicts[0].AgentB != "dev-worker-2" {
		t.Errorf("unexpected agents: %s, %s", conflicts[0].AgentA, conflicts[0].AgentB)
	}
}

func TestNoConflictDifferentFiles(t *testing.T) {
	hub := NewHub()

	hub.ReportFiles(&FileReport{
		Agent: "dev-worker-1", Team: "platform",
		Files: []string{"auth.go"},
	})
	hub.ReportFiles(&FileReport{
		Agent: "dev-worker-2", Team: "platform",
		Files: []string{"rate_limit.go"},
	})

	conflicts := hub.DetectConflicts("platform")
	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts, got %d", len(conflicts))
	}
}

func TestNoConflictDifferentTeams(t *testing.T) {
	hub := NewHub()

	hub.ReportFiles(&FileReport{
		Agent: "worker-1", Team: "alpha",
		Files: []string{"shared.go"},
	})
	hub.ReportFiles(&FileReport{
		Agent: "worker-2", Team: "beta",
		Files: []string{"shared.go"},
	})

	// Each team should have no conflicts
	if len(hub.DetectConflicts("alpha")) != 0 {
		t.Error("alpha should have no conflicts")
	}
	if len(hub.DetectConflicts("beta")) != 0 {
		t.Error("beta should have no conflicts")
	}
}

func TestScratchpad(t *testing.T) {
	hub := NewHub()

	hub.AppendScratchpad("platform", &ScratchpadEntry{
		From: "eng-manager",
		Type: "decision",
		Body: "Use jwt-go v5 for token validation",
	})

	hub.AppendScratchpad("platform", &ScratchpadEntry{
		From: "dev-worker-1",
		Type: "discovery",
		Body: "Found CVE in jwt-go v4",
	})

	entries := hub.GetScratchpad("platform")
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Type != "decision" {
		t.Errorf("expected first entry type 'decision', got %q", entries[0].Type)
	}
}

func TestScratchpadLimit(t *testing.T) {
	hub := NewHub()

	for i := 0; i < 110; i++ {
		hub.AppendScratchpad("test", &ScratchpadEntry{
			From: "agent",
			Body: "entry",
		})
	}

	entries := hub.GetScratchpad("test")
	if len(entries) != 100 {
		t.Errorf("expected 100 entries (capped), got %d", len(entries))
	}
}

func TestConflictAlertFormatting(t *testing.T) {
	c := FileConflict{
		File:    "api/handlers.go",
		AgentA:  "dev-worker-1",
		BranchA: "fix/auth",
		AgentB:  "dev-worker-2",
		BranchB: "feat/rate-limit",
	}

	alert := FormatConflictAlert(c, "dev-worker-1")
	if alert == "" {
		t.Fatal("expected non-empty alert")
	}
	// Should mention the OTHER agent
	if !containsStr(alert, "dev-worker-2") {
		t.Error("alert should mention the other agent")
	}
	if !containsStr(alert, "api/handlers.go") {
		t.Error("alert should mention the file")
	}
}

func TestFormatConflictSummary(t *testing.T) {
	conflicts := []FileConflict{
		{File: "a.go", AgentA: "w1", BranchA: "b1", AgentB: "w2", BranchB: "b2"},
		{File: "b.go", AgentA: "w1", BranchA: "b1", AgentB: "w2", BranchB: "b2"},
	}

	summary := FormatConflictSummary(conflicts, "w1")
	if summary == "" {
		t.Fatal("expected non-empty summary")
	}
	if !containsStr(summary, "a.go") || !containsStr(summary, "b.go") {
		t.Error("summary should mention both files")
	}

	// Empty conflicts
	empty := FormatConflictSummary(nil, "w1")
	if empty != "" {
		t.Error("expected empty string for no conflicts")
	}
}

func TestGetFiles(t *testing.T) {
	hub := NewHub()

	hub.ReportFiles(&FileReport{Agent: "w1", Team: "t", Files: []string{"a.go"}})
	hub.ReportFiles(&FileReport{Agent: "w2", Team: "t", Files: []string{"b.go"}})
	hub.ReportFiles(&FileReport{Agent: "w3", Team: "other", Files: []string{"c.go"}})

	files := hub.GetFiles("t")
	if len(files) != 2 {
		t.Fatalf("expected 2 reports for team t, got %d", len(files))
	}
}

func TestListMessages(t *testing.T) {
	hub := NewHub()

	hub.SendMessage(&TeamMessage{From: "a", Team: "t", Body: "hello"})
	hub.SendMessage(&TeamMessage{From: "b", Team: "t", Body: "world"})

	msgs := hub.ListMessages("t")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	// ListMessages is non-destructive
	msgs2 := hub.ListMessages("t")
	if len(msgs2) != 2 {
		t.Fatal("ListMessages should be non-destructive")
	}
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
