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
