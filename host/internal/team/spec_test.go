package team

import (
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
	yaml := `
apiVersion: boxcutter/v1
kind: Cluster
metadata:
  name: test
spec:
  agents:
    - name: a
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for wrong kind")
	}
}

func TestParse_MissingName(t *testing.T) {
	yaml := `
apiVersion: boxcutter/v1
kind: Team
metadata:
  name: ""
spec:
  agents:
    - name: a
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for empty metadata.name")
	}
}

func TestParse_NoAgents(t *testing.T) {
	yaml := `
apiVersion: boxcutter/v1
kind: Team
metadata:
  name: test
spec:
  agents: []
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for empty agents")
	}
}

func TestParse_AgentMissingName(t *testing.T) {
	yaml := `
apiVersion: boxcutter/v1
kind: Team
metadata:
  name: test
spec:
  agents:
    - replicas: 2
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for agent without name")
	}
}

func TestResolve_DefaultsApplied(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	agents := ts.Resolve()

	// eng-manager overrides ram and vcpu
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
	if dw.VCPU != 4 {
		t.Errorf("dev-worker VCPU = %d, want 4 (default)", dw.VCPU)
	}
}

func TestResolve_ReplicaExpansion(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	agents := ts.Resolve()

	// 1 eng-manager + 3 dev-workers + 1 product-manager = 5
	if len(agents) != 5 {
		t.Fatalf("resolved agents = %d, want 5", len(agents))
	}

	// Check dev-worker replica naming
	for i, idx := range []int{1, 2, 3} {
		a := agents[1+i]
		wantName := "platform-team-dev-worker-" + string(rune('0'+idx))
		if a.VMName != wantName {
			t.Errorf("replica %d name = %q, want %q", idx, a.VMName, wantName)
		}
		if a.ReplicaNum != idx {
			t.Errorf("replica %d num = %d", idx, a.ReplicaNum)
		}
	}
}

func TestResolve_AccessReplaces(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	agents := ts.Resolve()

	// eng-manager has agent-level access, should not inherit default repos
	em := agents[0]
	if len(em.Access) != 3 {
		t.Errorf("eng-manager access = %v, want 3 entries", em.Access)
	}

	// But repos come from defaults (eng-manager doesn't specify repos)
	if len(em.Repos) != 1 || em.Repos[0] != "AndrewBudd/boxcutter" {
		t.Errorf("eng-manager repos = %v, want [AndrewBudd/boxcutter] from defaults", em.Repos)
	}
}

func TestResolve_PersonaOverride(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	agents := ts.Resolve()

	em := agents[0]
	if em.Persona == nil || em.Persona.Role != "engineering-manager" {
		t.Errorf("eng-manager persona role = %v", em.Persona)
	}
}

func TestSummary(t *testing.T) {
	ts, _ := Parse([]byte(exampleYAML))
	s := ts.Summary()
	if s == "" {
		t.Fatal("summary should not be empty")
	}
	// Spot-check: should mention team name and VM names
	if !contains(s, "platform-team") {
		t.Error("summary missing team name")
	}
	if !contains(s, "platform-team-dev-worker-1") {
		t.Error("summary missing expanded replica name")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
