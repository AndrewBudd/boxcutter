package registry

import (
	"testing"
	"time"
)

func TestSetAndGetActivity(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	report := &ActivityReport{
		Timestamp:   time.Now(),
		PaneContent: "$ go test ./...",
		Status:      "active",
		Summary:     "running tests",
	}
	if !r.SetActivity("vm-1", report) {
		t.Fatal("SetActivity returned false")
	}

	got, ok := r.GetActivity("vm-1")
	if !ok {
		t.Fatal("GetActivity returned false")
	}
	if got.PaneContent != "$ go test ./..." {
		t.Fatalf("PaneContent = %q, want %q", got.PaneContent, "$ go test ./...")
	}
	if got.Summary != "running tests" {
		t.Fatalf("Summary = %q, want %q", got.Summary, "running tests")
	}
}

func TestSetActivityUnknownVM(t *testing.T) {
	r := New()
	report := &ActivityReport{Status: "active"}
	if r.SetActivity("nonexistent", report) {
		t.Fatal("SetActivity should return false for unknown VM")
	}
}

func TestPushAndPopMessages(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	msg1 := &Message{ID: "msg-1", From: "tapegun", Body: "do task A", Priority: "normal", CreatedAt: time.Now()}
	msg2 := &Message{ID: "msg-2", From: "tapegun", Body: "do task B", Priority: "urgent", CreatedAt: time.Now()}

	r.PushMessage("vm-1", msg1)
	r.PushMessage("vm-1", msg2)

	msgs, ok := r.PopUnread("vm-1")
	if !ok {
		t.Fatal("PopUnread returned false")
	}
	if len(msgs) != 2 {
		t.Fatalf("PopUnread returned %d messages, want 2", len(msgs))
	}
	if msgs[0].ID != "msg-1" || msgs[1].ID != "msg-2" {
		t.Fatalf("unexpected message order: %v", msgs)
	}

	// Second pop should return empty
	msgs, ok = r.PopUnread("vm-1")
	if !ok {
		t.Fatal("PopUnread returned false on second call")
	}
	if len(msgs) != 0 {
		t.Fatalf("PopUnread returned %d messages on second call, want 0", len(msgs))
	}
}

func TestPushMessageUnknownVM(t *testing.T) {
	r := New()
	msg := &Message{ID: "msg-1", Body: "test"}
	if r.PushMessage("nonexistent", msg) {
		t.Fatal("PushMessage should return false for unknown VM")
	}
}

func TestAckMessages(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	msg1 := &Message{ID: "msg-1", Body: "task A", CreatedAt: time.Now()}
	msg2 := &Message{ID: "msg-2", Body: "task B", CreatedAt: time.Now()}
	r.PushMessage("vm-1", msg1)
	r.PushMessage("vm-1", msg2)

	// Ack only msg-1
	r.AckMessages("vm-1", []string{"msg-1"})

	// PopUnread should return only msg-2
	msgs, _ := r.PopUnread("vm-1")
	if len(msgs) != 1 {
		t.Fatalf("PopUnread returned %d messages, want 1", len(msgs))
	}
	if msgs[0].ID != "msg-2" {
		t.Fatalf("expected msg-2, got %s", msgs[0].ID)
	}
}

func TestPeekInbox(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	msg := &Message{ID: "msg-1", Body: "peek test", CreatedAt: time.Now()}
	r.PushMessage("vm-1", msg)

	// Peek should not modify inbox
	msgs, ok := r.PeekInbox("vm-1")
	if !ok {
		t.Fatal("PeekInbox returned false")
	}
	if len(msgs) != 1 {
		t.Fatalf("PeekInbox returned %d messages, want 1", len(msgs))
	}

	// Peek again should still return the same message
	msgs, _ = r.PeekInbox("vm-1")
	if len(msgs) != 1 {
		t.Fatalf("second PeekInbox returned %d messages, want 1", len(msgs))
	}
}

func TestAllActivity(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})
	r.Register(&VMRecord{VMID: "vm-2", IP: "10.0.0.2", Mark: 200})

	r.SetActivity("vm-1", &ActivityReport{Status: "active", PaneContent: "working"})
	r.PushMessage("vm-2", &Message{ID: "msg-1", Body: "do X", CreatedAt: time.Now()})

	summaries := r.AllActivity()
	if len(summaries) != 2 {
		t.Fatalf("AllActivity returned %d summaries, want 2", len(summaries))
	}

	byID := make(map[string]VMActivitySummary)
	for _, s := range summaries {
		byID[s.VMID] = s
	}

	if byID["vm-1"].LastActivity == nil {
		t.Fatal("vm-1 should have activity")
	}
	if byID["vm-1"].LastActivity.Status != "active" {
		t.Fatalf("vm-1 status = %q, want %q", byID["vm-1"].LastActivity.Status, "active")
	}
	if byID["vm-2"].PendingMessages != 1 {
		t.Fatalf("vm-2 pending = %d, want 1", byID["vm-2"].PendingMessages)
	}
}

func TestPendingCount(t *testing.T) {
	rec := &VMRecord{VMID: "vm-1"}
	if rec.PendingCount() != 0 {
		t.Fatalf("PendingCount = %d, want 0", rec.PendingCount())
	}

	now := time.Now()
	rec.PushMessage(&Message{ID: "1", CreatedAt: now})
	rec.PushMessage(&Message{ID: "2", CreatedAt: now})
	if rec.PendingCount() != 2 {
		t.Fatalf("PendingCount = %d, want 2", rec.PendingCount())
	}

	rec.AckMessages([]string{"1"})
	if rec.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1", rec.PendingCount())
	}
}
