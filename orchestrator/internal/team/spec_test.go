package team

import (
	"strings"
	"testing"
)

const exampleYAML = `
apiVersion: boxcutter/v1
kind: Team
metadata:
  name: platform-team
  labels:
    project: boxcutter
spec:
  defaults:
    type: firecracker
    ram: 6G
    vcpu: 4
    mode: normal
    repos:
      - AndrewBudd/boxcutter
    authorized: true

  agents:
    - name: eng-manager
      ram: 4G
      vcpu: 2
      persona:
        role: engineering-manager
        claude_md: .claude/personas/eng-manager.md
        instructions: |
          You coordinate dev workers.
      access:
        - github-issues
        - github-prs
        - tapegun-sendkeys

    - name: dev-worker
      replicas: 3
      persona:
        role: developer
        claude_md: .claude/personas/developer.md
      access:
        - github-issues
        - github-prs

    - name: product-manager
      ram: 4G
      vcpu: 2
      persona:
        role: product-manager
        claude_md: .claude/personas/product-manager.md
        instructions: |
          You write specs, prioritize backlog.
      access:
        - github-issues
        - github-projects
`

func TestParse_ValidYAML(t *testing.T) {
	ts, err := Parse([]byte(exampleYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ts.Metadata.Name != "platform-team" {
		t.Errorf("name = %q, want platform-team", ts.Metadata.Name)
	}
	if len(ts.Spec.Agents) != 3 {
		t.Errorf("agents = %d, want 3", len(ts.Spec.Agents))
	}
	if ts.Spec.Agents[1].Replicas != 3 {
		t.Errorf("dev-worker replicas = %d, want 3", ts.Spec.Agents[1].Replicas)
	}
}

func TestParse_InvalidKind(t *testing.T) {
	_, err := Parse([]byte(`
apiVersion: boxcutter/v1
kind: Cluster
metadata:
  name: test
spec:
  agents:
    - name: a
`))
	if err == nil {
		t.Fatal("expected error for wrong kind")
	}
}

func TestParse_MissingName(t *testing.T) {
	_, err := Parse([]byte(`
apiVersion: boxcutter/v1
kind: Team
metadata:
  name: ""
spec:
  agents:
    - name: a
`))
	if err == nil {
		t.Fatal("expected error for empty metadata.name")
	}
}

func TestParse_NoAgents(t *testing.T) {
	_, err := Parse([]byte(`
apiVersion: boxcutter/v1
kind: Team
metadata:
  name: test
spec:
  agents: []
`))
	if err == nil {
		t.Fatal("expected error for empty agents")
	}
}

func TestResolve_DefaultsApplied(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	agents := ts.Resolve()

	em := agents[0]
	if em.VMName != "platform-team-eng-manager-1" {
		t.Errorf("vmname = %q", em.VMName)
	}
	if em.RAM != "4G" {
		t.Errorf("eng-manager RAM = %q, want 4G (override)", em.RAM)
	}
	if em.VCPU != 2 {
		t.Errorf("eng-manager VCPU = %d, want 2 (override)", em.VCPU)
	}
	if em.Type != "firecracker" {
		t.Errorf("eng-manager type = %q, want firecracker (default)", em.Type)
	}
	if !em.Authorized {
		t.Error("eng-manager should inherit authorized=true from defaults")
	}

	// dev-worker inherits all defaults
	dw := agents[1]
	if dw.RAM != "6G" {
		t.Errorf("dev-worker RAM = %q, want 6G (default)", dw.RAM)
	}
}

func TestResolve_ReplicaExpansion(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	agents := ts.Resolve()

	// 1 eng-manager + 3 dev-workers + 1 product-manager = 5
	if len(agents) != 5 {
		t.Fatalf("resolved agents = %d, want 5", len(agents))
	}

	for i := 0; i < 3; i++ {
		a := agents[1+i]
		wantNum := i + 1
		if a.ReplicaNum != wantNum {
			t.Errorf("replica %d num = %d", wantNum, a.ReplicaNum)
		}
		if a.AgentName != "dev-worker" {
			t.Errorf("replica %d agent = %q, want dev-worker", wantNum, a.AgentName)
		}
	}
}

func TestResolve_Labels(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	agents := ts.Resolve()

	labels := agents[0].Labels("platform-team")
	if labels["team"] != "platform-team" {
		t.Errorf("team label = %q", labels["team"])
	}
	if labels["agent"] != "eng-manager" {
		t.Errorf("agent label = %q", labels["agent"])
	}
	if labels["role"] != "engineering-manager" {
		t.Errorf("role label = %q", labels["role"])
	}
}

func TestParseRAMMiB(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"4G", 4096},
		{"6G", 6144},
		{"2048M", 2048},
		{"512m", 512},
		{"2048", 2048},
		{"", 0},
	}
	for _, tt := range tests {
		got := ParseRAMMiB(tt.input)
		if got != tt.want {
			t.Errorf("ParseRAMMiB(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestResolve_CloneURLs(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	agents := ts.Resolve()

	urls := agents[0].CloneURLs()
	if len(urls) != 1 {
		t.Fatalf("clone urls = %d, want 1", len(urls))
	}
	if !strings.Contains(urls[0], "github.com/AndrewBudd/boxcutter") {
		t.Errorf("clone url = %q", urls[0])
	}
}

func TestResolve_PersonaParsed(t *testing.T) {
	ts, err := Parse([]byte(exampleYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	agents := ts.Resolve()

	// eng-manager has persona with role, claude_md, and instructions
	em := agents[0]
	if em.Persona == nil {
		t.Fatal("eng-manager persona is nil")
	}
	if em.Persona.Role != "engineering-manager" {
		t.Errorf("role = %q, want engineering-manager", em.Persona.Role)
	}
	if em.Persona.ClaudeMD != ".claude/personas/eng-manager.md" {
		t.Errorf("claude_md = %q", em.Persona.ClaudeMD)
	}
	if !strings.Contains(em.Persona.Instructions, "coordinate dev workers") {
		t.Errorf("instructions = %q, want to contain 'coordinate dev workers'", em.Persona.Instructions)
	}

	// dev-worker has persona with role + claude_md but no instructions
	dw := agents[1]
	if dw.Persona == nil {
		t.Fatal("dev-worker persona is nil")
	}
	if dw.Persona.Role != "developer" {
		t.Errorf("role = %q, want developer", dw.Persona.Role)
	}
	if dw.Persona.ClaudeMD != ".claude/personas/developer.md" {
		t.Errorf("claude_md = %q", dw.Persona.ClaudeMD)
	}
	if dw.Persona.Instructions != "" {
		t.Errorf("dev-worker should have no instructions, got %q", dw.Persona.Instructions)
	}
}

func TestResolve_PersonaInAgentConfig(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	agents := ts.Resolve()

	cfg := agents[0].AgentConfig("platform-team", nil)

	persona, ok := cfg["persona"].(map[string]interface{})
	if !ok {
		t.Fatal("agent-config missing persona block")
	}
	if persona["role"] != "engineering-manager" {
		t.Errorf("agent-config persona.role = %v", persona["role"])
	}
	if persona["claude_md"] != ".claude/personas/eng-manager.md" {
		t.Errorf("agent-config persona.claude_md = %v", persona["claude_md"])
	}
	if _, ok := persona["instructions"]; !ok {
		t.Error("agent-config persona missing instructions")
	}
}

func TestResolve_PersonaDefaultInheritance(t *testing.T) {
	yaml := `
apiVersion: boxcutter/v1
kind: Team
metadata:
  name: test
spec:
  defaults:
    persona:
      role: default-role
      claude_md: .claude/personas/default.md
      instructions: default instructions
  agents:
    - name: inherits-all
    - name: overrides-role
      persona:
        role: custom-role
        claude_md: .claude/personas/custom.md
`
	ts, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	agents := ts.Resolve()

	// First agent inherits defaults
	a := agents[0]
	if a.Persona == nil {
		t.Fatal("inherits-all: persona is nil")
	}
	if a.Persona.Role != "default-role" {
		t.Errorf("inherits-all: role = %q, want default-role", a.Persona.Role)
	}
	if a.Persona.ClaudeMD != ".claude/personas/default.md" {
		t.Errorf("inherits-all: claude_md = %q", a.Persona.ClaudeMD)
	}
	if a.Persona.Instructions != "default instructions" {
		t.Errorf("inherits-all: instructions = %q", a.Persona.Instructions)
	}

	// Second agent overrides persona entirely (replace, not merge)
	b := agents[1]
	if b.Persona == nil {
		t.Fatal("overrides-role: persona is nil")
	}
	if b.Persona.Role != "custom-role" {
		t.Errorf("overrides-role: role = %q, want custom-role", b.Persona.Role)
	}
	if b.Persona.Instructions != "" {
		t.Errorf("overrides-role: instructions should be empty (override replaces), got %q", b.Persona.Instructions)
	}
}

func TestResolve_TeamPersonaFiles(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	agents := ts.Resolve()

	allFiles := []string{".claude/personas/eng-manager.md", ".claude/personas/developer.md", ".claude/personas/product-manager.md"}
	cfg := agents[0].AgentConfig("platform-team", allFiles)

	files, ok := cfg["team_persona_files"].([]string)
	if !ok {
		t.Fatal("agent-config missing team_persona_files")
	}
	if len(files) != 3 {
		t.Errorf("team_persona_files = %d, want 3", len(files))
	}
}

func TestResolve_Idempotent(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	a1 := ts.Resolve()
	a2 := ts.Resolve()
	if len(a1) != len(a2) {
		t.Fatalf("resolve not idempotent: %d vs %d", len(a1), len(a2))
	}
	for i := range a1 {
		if a1[i].VMName != a2[i].VMName {
			t.Errorf("resolve[%d] name mismatch: %q vs %q", i, a1[i].VMName, a2[i].VMName)
		}
	}
}
