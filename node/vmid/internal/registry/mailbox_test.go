package registry

import (
	"fmt"
	"testing"
	"time"
)

func TestMailbox_PushAndConsume(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	msg := &MailboxMessage{
		ID:        "msg-1",
		From:      "vm-2",
		To:        "vm-1",
		Subject:   "hello",
		Body:      "world",
		CreatedAt: time.Now(),
	}

	if !r.PushMailbox("vm-1", msg) {
		t.Fatal("PushMailbox should succeed for registered VM")
	}

	msgs, ok := r.ConsumeMailbox("vm-1")
	if !ok {
		t.Fatal("ConsumeMailbox should succeed for registered VM")
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].ID != "msg-1" {
		t.Errorf("message ID = %q, want msg-1", msgs[0].ID)
	}
	if msgs[0].From != "vm-2" {
		t.Errorf("message From = %q, want vm-2", msgs[0].From)
	}
	if msgs[0].Subject != "hello" {
		t.Errorf("message Subject = %q, want hello", msgs[0].Subject)
	}
}

func TestMailbox_ConsumeMarksInFlight(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	r.PushMailbox("vm-1", &MailboxMessage{ID: "msg-1", From: "vm-2", To: "vm-1", Body: "a", CreatedAt: time.Now()})
	r.PushMailbox("vm-1", &MailboxMessage{ID: "msg-2", From: "vm-3", To: "vm-1", Body: "b", CreatedAt: time.Now()})

	// First consume should return both
	msgs, _ := r.ConsumeMailbox("vm-1")
	if len(msgs) != 2 {
		t.Fatalf("first consume: expected 2 messages, got %d", len(msgs))
	}

	// Second consume should return nothing (all in-flight)
	msgs2, _ := r.ConsumeMailbox("vm-1")
	if len(msgs2) != 0 {
		t.Fatalf("second consume: expected 0 messages (in-flight), got %d", len(msgs2))
	}
}

func TestMailbox_AckDeletesMessage(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	r.PushMailbox("vm-1", &MailboxMessage{ID: "msg-1", From: "vm-2", To: "vm-1", Body: "a", CreatedAt: time.Now()})
	r.PushMailbox("vm-1", &MailboxMessage{ID: "msg-2", From: "vm-3", To: "vm-1", Body: "b", CreatedAt: time.Now()})

	// Consume both
	r.ConsumeMailbox("vm-1")

	// Ack one
	if !r.AckMailbox("vm-1", "msg-1") {
		t.Fatal("AckMailbox should succeed for existing message")
	}

	// Ack the same one again — should fail (already deleted)
	if r.AckMailbox("vm-1", "msg-1") {
		t.Fatal("AckMailbox should fail for already-deleted message")
	}

	// msg-2 should still exist (in-flight)
	rec, _ := r.LookupID("vm-1")
	if len(rec.Mailbox) != 1 {
		t.Fatalf("expected 1 remaining message, got %d", len(rec.Mailbox))
	}
	if rec.Mailbox[0].ID != "msg-2" {
		t.Errorf("remaining message ID = %q, want msg-2", rec.Mailbox[0].ID)
	}
}

func TestMailbox_InFlightTimeout(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	r.PushMailbox("vm-1", &MailboxMessage{ID: "msg-1", From: "vm-2", To: "vm-1", Body: "a", CreatedAt: time.Now()})

	// Consume to mark in-flight
	msgs, _ := r.ConsumeMailbox("vm-1")
	if len(msgs) != 1 {
		t.Fatal("first consume should return 1")
	}

	// Manually expire the in-flight timestamp
	rec, _ := r.LookupID("vm-1")
	expired := time.Now().Add(-InFlightTimeout - time.Second)
	rec.Mailbox[0].InFlight = &expired

	// Consume again — should redeliver the expired message
	msgs2, _ := r.ConsumeMailbox("vm-1")
	if len(msgs2) != 1 {
		t.Fatalf("expected 1 redelivered message, got %d", len(msgs2))
	}
	if msgs2[0].ID != "msg-1" {
		t.Errorf("redelivered message ID = %q, want msg-1", msgs2[0].ID)
	}
}

func TestMailbox_InFlightNotExpired(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	r.PushMailbox("vm-1", &MailboxMessage{ID: "msg-1", From: "vm-2", To: "vm-1", Body: "a", CreatedAt: time.Now()})

	// Consume
	r.ConsumeMailbox("vm-1")

	// Without expiring, second consume should still return empty
	msgs, _ := r.ConsumeMailbox("vm-1")
	if len(msgs) != 0 {
		t.Fatalf("should not redeliver non-expired in-flight message, got %d", len(msgs))
	}
}

func TestMailbox_PushToUnknownVM(t *testing.T) {
	r := New()
	msg := &MailboxMessage{ID: "msg-1", From: "vm-2", To: "vm-unknown", Body: "a", CreatedAt: time.Now()}
	if r.PushMailbox("vm-unknown", msg) {
		t.Fatal("PushMailbox should fail for unregistered VM")
	}
}

func TestMailbox_ConsumeUnknownVM(t *testing.T) {
	r := New()
	_, ok := r.ConsumeMailbox("vm-unknown")
	if ok {
		t.Fatal("ConsumeMailbox should fail for unregistered VM")
	}
}

func TestMailbox_AckUnknownVM(t *testing.T) {
	r := New()
	if r.AckMailbox("vm-unknown", "msg-1") {
		t.Fatal("AckMailbox should fail for unregistered VM")
	}
}

func TestMailbox_AckNonexistentMessage(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})
	r.PushMailbox("vm-1", &MailboxMessage{ID: "msg-1", From: "vm-2", To: "vm-1", Body: "a", CreatedAt: time.Now()})

	if r.AckMailbox("vm-1", "msg-999") {
		t.Fatal("AckMailbox should fail for nonexistent message ID")
	}
}

func TestMailbox_ExportForMigration(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	r.PushMailbox("vm-1", &MailboxMessage{ID: "msg-1", From: "vm-2", To: "vm-1", Body: "a", CreatedAt: time.Now()})
	r.PushMailbox("vm-1", &MailboxMessage{ID: "msg-2", From: "vm-3", To: "vm-1", Body: "b", CreatedAt: time.Now()})

	// Consume to mark in-flight
	r.ConsumeMailbox("vm-1")

	// Export should include all messages with InFlight reset
	exported, ok := r.ExportMailbox("vm-1")
	if !ok {
		t.Fatal("ExportMailbox should succeed")
	}
	if len(exported) != 2 {
		t.Fatalf("expected 2 exported messages, got %d", len(exported))
	}
	for _, m := range exported {
		if m.InFlight != nil {
			t.Errorf("exported message %s should have InFlight=nil, got %v", m.ID, m.InFlight)
		}
	}
}

func TestMailbox_ExportEmpty(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	exported, ok := r.ExportMailbox("vm-1")
	if !ok {
		t.Fatal("ExportMailbox should succeed for empty mailbox")
	}
	if exported != nil {
		t.Errorf("empty mailbox export should be nil, got %v", exported)
	}
}

func TestMailbox_ImportAfterMigration(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	msgs := []*MailboxMessage{
		{ID: "msg-1", From: "vm-2", To: "vm-1", Body: "migrated-a", CreatedAt: time.Now()},
		{ID: "msg-2", From: "vm-3", To: "vm-1", Body: "migrated-b", CreatedAt: time.Now()},
	}

	if !r.ImportMailbox("vm-1", msgs) {
		t.Fatal("ImportMailbox should succeed")
	}

	// Should be consumable
	consumed, _ := r.ConsumeMailbox("vm-1")
	if len(consumed) != 2 {
		t.Fatalf("expected 2 imported messages, got %d", len(consumed))
	}
}

func TestMailbox_DeregisterCleansUp(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	r.PushMailbox("vm-1", &MailboxMessage{ID: "msg-1", From: "vm-2", To: "vm-1", Body: "a", CreatedAt: time.Now()})

	r.Deregister("vm-1")

	// Messages should be gone with the VM
	_, ok := r.ConsumeMailbox("vm-1")
	if ok {
		t.Fatal("ConsumeMailbox should fail after deregister")
	}
}

func TestMailbox_LocalDelivery(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "sender", IP: "10.0.0.2", Mark: 100})
	r.Register(&VMRecord{VMID: "receiver", IP: "10.0.0.2", Mark: 200})

	msg := &MailboxMessage{
		ID:        "msg-1",
		From:      "sender",
		To:        "receiver",
		Body:      "local message",
		CreatedAt: time.Now(),
	}

	// Push to receiver's mailbox (simulates local delivery)
	if !r.PushMailbox("receiver", msg) {
		t.Fatal("local delivery should succeed")
	}

	// Receiver should be able to consume it
	msgs, _ := r.ConsumeMailbox("receiver")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].From != "sender" {
		t.Errorf("message from = %q, want sender", msgs[0].From)
	}
}

func TestMailbox_MultipleConsumersOneVM(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	for i := 0; i < 5; i++ {
		r.PushMailbox("vm-1", &MailboxMessage{
			ID: fmt.Sprintf("msg-%d", i), From: "vm-2", To: "vm-1",
			Body: fmt.Sprintf("msg %d", i), CreatedAt: time.Now(),
		})
	}

	// First consume gets all 5
	msgs1, _ := r.ConsumeMailbox("vm-1")
	if len(msgs1) != 5 {
		t.Fatalf("first consume: expected 5, got %d", len(msgs1))
	}

	// Ack first 3
	for i := 0; i < 3; i++ {
		r.AckMailbox("vm-1", fmt.Sprintf("msg-%d", i))
	}

	// Expire remaining 2
	rec, _ := r.LookupID("vm-1")
	for _, m := range rec.Mailbox {
		expired := time.Now().Add(-InFlightTimeout - time.Second)
		m.InFlight = &expired
	}

	// Second consume should get the 2 redelivered
	msgs2, _ := r.ConsumeMailbox("vm-1")
	if len(msgs2) != 2 {
		t.Fatalf("second consume: expected 2 redelivered, got %d", len(msgs2))
	}
}

func TestMailbox_ExportDoesNotMutateOriginal(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	r.PushMailbox("vm-1", &MailboxMessage{ID: "msg-1", From: "vm-2", To: "vm-1", Body: "a", CreatedAt: time.Now()})
	r.ConsumeMailbox("vm-1") // mark in-flight

	// Export resets InFlight
	exported, _ := r.ExportMailbox("vm-1")
	if exported[0].InFlight != nil {
		t.Fatal("exported should have nil InFlight")
	}

	// Original should still be in-flight
	rec, _ := r.LookupID("vm-1")
	if rec.Mailbox[0].InFlight == nil {
		t.Fatal("original should still be in-flight after export")
	}
}
