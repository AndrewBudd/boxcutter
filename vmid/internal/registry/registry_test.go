package registry

import (
	"testing"
)

func TestRegisterAndLookupMark(t *testing.T) {
	r := New()

	rec := &VMRecord{
		VMID: "test-vm",
		IP:   "10.0.0.2",
		Mark: 12345,
		Mode: "normal",
	}
	r.Register(rec)

	// Lookup by mark
	found, ok := r.LookupMark(12345)
	if !ok {
		t.Fatal("LookupMark returned false")
	}
	if found.VMID != "test-vm" {
		t.Fatalf("LookupMark returned VMID %q, want %q", found.VMID, "test-vm")
	}
	if found.Mode != "normal" {
		t.Fatalf("Mode = %q, want %q", found.Mode, "normal")
	}

	// Lookup by ID still works
	found, ok = r.LookupID("test-vm")
	if !ok {
		t.Fatal("LookupID returned false")
	}
	if found.Mark != 12345 {
		t.Fatalf("Mark = %d, want %d", found.Mark, 12345)
	}

	// Lookup by IP still works
	found, ok = r.LookupIP("10.0.0.2")
	if !ok {
		t.Fatal("LookupIP returned false")
	}
	if found.VMID != "test-vm" {
		t.Fatalf("LookupIP returned VMID %q, want %q", found.VMID, "test-vm")
	}
}

func TestDeregisterClearsMark(t *testing.T) {
	r := New()

	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100, Mode: "paranoid"})

	ok := r.Deregister("vm-1")
	if !ok {
		t.Fatal("Deregister returned false")
	}

	// Mark should be gone
	_, ok = r.LookupMark(100)
	if ok {
		t.Fatal("LookupMark should return false after deregister")
	}
}

func TestMultipleVMsDifferentMarks(t *testing.T) {
	r := New()

	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100, Mode: "normal"})
	r.Register(&VMRecord{VMID: "vm-2", IP: "10.0.0.2", Mark: 200, Mode: "paranoid"})

	rec1, ok := r.LookupMark(100)
	if !ok || rec1.VMID != "vm-1" {
		t.Fatalf("LookupMark(100) = %v, %v", rec1, ok)
	}

	rec2, ok := r.LookupMark(200)
	if !ok || rec2.VMID != "vm-2" {
		t.Fatalf("LookupMark(200) = %v, %v", rec2, ok)
	}

	// Both share 10.0.0.2 — IP lookup returns last registered
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("List() = %d items, want 2", len(list))
	}
}

func TestZeroMarkNotIndexed(t *testing.T) {
	r := New()

	r.Register(&VMRecord{VMID: "vm-no-mark", IP: "10.0.0.2", Mark: 0})

	_, ok := r.LookupMark(0)
	if ok {
		t.Fatal("LookupMark(0) should return false")
	}

	// But should still be findable by ID
	_, ok = r.LookupID("vm-no-mark")
	if !ok {
		t.Fatal("LookupID should still work")
	}
}
