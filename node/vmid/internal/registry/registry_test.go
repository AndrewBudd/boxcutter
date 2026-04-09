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

func TestAgentConfig_SetAndGet(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	cfg := &AgentConfig{
		Persona: "backend-eng",
		Repos:   []string{"org/repo1", "org/repo2"},
		Flags:   map[string]string{"verbose": "true"},
	}
	if !r.SetAgentConfig("vm-1", cfg) {
		t.Fatal("SetAgentConfig returned false")
	}

	got, ok := r.GetAgentConfig("vm-1")
	if !ok {
		t.Fatal("GetAgentConfig returned false")
	}
	if got.Persona != "backend-eng" {
		t.Fatalf("Persona = %q, want %q", got.Persona, "backend-eng")
	}
	if len(got.Repos) != 2 || got.Repos[0] != "org/repo1" {
		t.Fatalf("Repos = %v, want [org/repo1 org/repo2]", got.Repos)
	}
	if got.Flags["verbose"] != "true" {
		t.Fatalf("Flags = %v, want verbose=true", got.Flags)
	}
}

func TestAgentConfig_NotFound(t *testing.T) {
	r := New()
	_, ok := r.GetAgentConfig("nonexistent")
	if ok {
		t.Fatal("GetAgentConfig should return false for unknown VM")
	}
	if r.SetAgentConfig("nonexistent", &AgentConfig{Persona: "x"}) {
		t.Fatal("SetAgentConfig should return false for unknown VM")
	}
}

func TestAgentConfig_NilByDefault(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	cfg, ok := r.GetAgentConfig("vm-1")
	if !ok {
		t.Fatal("GetAgentConfig returned false for registered VM")
	}
	if cfg != nil {
		t.Fatalf("AgentConfig should be nil by default, got %v", cfg)
	}
}

func TestAgentConfig_SetAtRegistration(t *testing.T) {
	r := New()
	r.Register(&VMRecord{
		VMID: "vm-1",
		IP:   "10.0.0.2",
		Mark: 100,
		AgentConfig: &AgentConfig{
			Persona: "security-reviewer",
			Repos:   []string{"org/sec-tools"},
		},
	})

	cfg, ok := r.GetAgentConfig("vm-1")
	if !ok || cfg == nil {
		t.Fatal("AgentConfig should be set from registration")
	}
	if cfg.Persona != "security-reviewer" {
		t.Fatalf("Persona = %q, want %q", cfg.Persona, "security-reviewer")
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
