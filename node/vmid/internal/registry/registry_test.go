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
		Team:         "platform",
		Agent:        "backend-eng",
		ReplicaIndex: 0,
		Persona: &PersonaConfig{
			Role:         "backend-eng",
			ClaudeMD:     "# Backend Engineer\nFocus on Go services.",
			Instructions: "Run tests before committing.",
		},
		Repos:      []string{"org/repo1", "org/repo2"},
		Tapegun:    []string{"setup-env", "clone-repos"},
		Access:     []string{"github:org/repo1", "github:org/repo2"},
		Authorized: true,
		VMConfig: &VMConfigInfo{
			VMType: "firecracker",
			CPUs:   2,
			MemMB:  2048,
			DiskGB: 10,
		},
		Labels:           map[string]string{"env": "dev", "team": "platform"},
		TeamPersonaFiles: []string{"/team/shared-claude.md"},
	}
	if !r.SetAgentConfig("vm-1", cfg) {
		t.Fatal("SetAgentConfig returned false")
	}

	got, ok := r.GetAgentConfig("vm-1")
	if !ok {
		t.Fatal("GetAgentConfig returned false")
	}
	if got.Team != "platform" {
		t.Fatalf("Team = %q, want %q", got.Team, "platform")
	}
	if got.Agent != "backend-eng" {
		t.Fatalf("Agent = %q, want %q", got.Agent, "backend-eng")
	}
	if got.Persona == nil || got.Persona.Role != "backend-eng" {
		t.Fatalf("Persona.Role = %v, want %q", got.Persona, "backend-eng")
	}
	if got.Persona.ClaudeMD != "# Backend Engineer\nFocus on Go services." {
		t.Fatalf("Persona.ClaudeMD = %q, want non-empty", got.Persona.ClaudeMD)
	}
	if len(got.Repos) != 2 || got.Repos[0] != "org/repo1" {
		t.Fatalf("Repos = %v, want [org/repo1 org/repo2]", got.Repos)
	}
	if len(got.Tapegun) != 2 || got.Tapegun[0] != "setup-env" {
		t.Fatalf("Tapegun = %v, want [setup-env clone-repos]", got.Tapegun)
	}
	if !got.Authorized {
		t.Fatal("Authorized = false, want true")
	}
	if got.VMConfig == nil || got.VMConfig.CPUs != 2 {
		t.Fatalf("VMConfig.CPUs = %v, want 2", got.VMConfig)
	}
	if got.Labels["team"] != "platform" {
		t.Fatalf("Labels = %v, want team=platform", got.Labels)
	}
	if len(got.TeamPersonaFiles) != 1 || got.TeamPersonaFiles[0] != "/team/shared-claude.md" {
		t.Fatalf("TeamPersonaFiles = %v, want [/team/shared-claude.md]", got.TeamPersonaFiles)
	}
}

func TestAgentConfig_NotFound(t *testing.T) {
	r := New()
	_, ok := r.GetAgentConfig("nonexistent")
	if ok {
		t.Fatal("GetAgentConfig should return false for unknown VM")
	}
	if r.SetAgentConfig("nonexistent", &AgentConfig{Agent: "x"}) {
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
			Team:  "security",
			Agent: "security-reviewer",
			Persona: &PersonaConfig{
				Role:     "security-reviewer",
				ClaudeMD: "# Security Reviewer",
			},
			Repos:      []string{"org/sec-tools"},
			Authorized: false,
		},
	})

	cfg, ok := r.GetAgentConfig("vm-1")
	if !ok || cfg == nil {
		t.Fatal("AgentConfig should be set from registration")
	}
	if cfg.Team != "security" {
		t.Fatalf("Team = %q, want %q", cfg.Team, "security")
	}
	if cfg.Agent != "security-reviewer" {
		t.Fatalf("Agent = %q, want %q", cfg.Agent, "security-reviewer")
	}
	if cfg.Persona == nil || cfg.Persona.Role != "security-reviewer" {
		t.Fatalf("Persona.Role = %v, want %q", cfg.Persona, "security-reviewer")
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
