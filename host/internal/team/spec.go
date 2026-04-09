package team

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// TeamSpec is the top-level structure for a team YAML file.
type TeamSpec struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

type Metadata struct {
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels,omitempty"`
}

type Spec struct {
	Defaults *AgentDefaults `yaml:"defaults,omitempty"`
	Agents   []AgentSpec    `yaml:"agents"`
}

// AgentDefaults holds shared defaults inherited by all agents.
// Agent-level values override defaults. List fields (repos, access)
// are replaced, not merged.
type AgentDefaults struct {
	Type       string  `yaml:"type,omitempty"`
	RAM        string  `yaml:"ram,omitempty"`
	VCPU       int     `yaml:"vcpu,omitempty"`
	Disk       string  `yaml:"disk,omitempty"`
	Mode       string  `yaml:"mode,omitempty"`
	Repos      []string `yaml:"repos,omitempty"`
	Authorized bool    `yaml:"authorized,omitempty"`
	Persona    *Persona `yaml:"persona,omitempty"`
}

type AgentSpec struct {
	Name        string   `yaml:"name"`
	Replicas    int      `yaml:"replicas,omitempty"`
	RAM         string   `yaml:"ram,omitempty"`
	VCPU        int      `yaml:"vcpu,omitempty"`
	Disk        string   `yaml:"disk,omitempty"`
	Type        string   `yaml:"type,omitempty"`
	Mode        string   `yaml:"mode,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Persona     *Persona `yaml:"persona,omitempty"`
	Repos       []string `yaml:"repos,omitempty"`
	Access      []string `yaml:"access,omitempty"`
	Authorized  *bool    `yaml:"authorized,omitempty"`
}

type Persona struct {
	Role         string `yaml:"role,omitempty"`
	ClaudeMD     string `yaml:"claude_md,omitempty"`
	Instructions string `yaml:"instructions,omitempty"`
}

// LoadFile parses a team YAML file from disk.
func LoadFile(path string) (*TeamSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return Parse(data)
}

// Parse parses team YAML bytes.
func Parse(data []byte) (*TeamSpec, error) {
	var ts TeamSpec
	if err := yaml.Unmarshal(data, &ts); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	if ts.Kind != "Team" {
		return nil, fmt.Errorf("expected kind: Team, got %q", ts.Kind)
	}
	if ts.Metadata.Name == "" {
		return nil, fmt.Errorf("metadata.name is required")
	}
	if len(ts.Spec.Agents) == 0 {
		return nil, fmt.Errorf("spec.agents must contain at least one agent")
	}
	for i, a := range ts.Spec.Agents {
		if a.Name == "" {
			return nil, fmt.Errorf("spec.agents[%d].name is required", i)
		}
	}
	return &ts, nil
}

// ResolvedAgent is an agent spec with defaults applied and replicas expanded.
type ResolvedAgent struct {
	VMName      string
	AgentName   string
	ReplicaNum  int
	Type        string
	RAM         string
	VCPU        int
	Disk        string
	Mode        string
	Description string
	Persona     *Persona
	Repos       []string
	Access      []string
	Authorized  bool
}

// Resolve applies defaults and expands replicas into concrete VM definitions.
func (ts *TeamSpec) Resolve() []ResolvedAgent {
	var out []ResolvedAgent
	for _, a := range ts.Spec.Agents {
		replicas := a.Replicas
		if replicas < 1 {
			replicas = 1
		}
		for i := 1; i <= replicas; i++ {
			r := ResolvedAgent{
				VMName:      fmt.Sprintf("%s-%s-%d", ts.Metadata.Name, a.Name, i),
				AgentName:   a.Name,
				ReplicaNum:  i,
				Description: a.Description,
			}

			// Apply defaults, then agent overrides
			d := ts.Spec.Defaults

			r.Type = "firecracker"
			r.RAM = "6G"
			r.VCPU = 4
			r.Disk = "50G"
			r.Mode = "normal"

			if d != nil {
				if d.Type != "" {
					r.Type = d.Type
				}
				if d.RAM != "" {
					r.RAM = d.RAM
				}
				if d.VCPU > 0 {
					r.VCPU = d.VCPU
				}
				if d.Disk != "" {
					r.Disk = d.Disk
				}
				if d.Mode != "" {
					r.Mode = d.Mode
				}
				r.Repos = d.Repos
				r.Authorized = d.Authorized
				if d.Persona != nil {
					p := *d.Persona
					r.Persona = &p
				}
			}

			// Agent-level overrides (replace, not merge)
			if a.Type != "" {
				r.Type = a.Type
			}
			if a.RAM != "" {
				r.RAM = a.RAM
			}
			if a.VCPU > 0 {
				r.VCPU = a.VCPU
			}
			if a.Disk != "" {
				r.Disk = a.Disk
			}
			if a.Mode != "" {
				r.Mode = a.Mode
			}
			if a.Repos != nil {
				r.Repos = a.Repos
			}
			if a.Access != nil {
				r.Access = a.Access
			}
			if a.Authorized != nil {
				r.Authorized = *a.Authorized
			}
			if a.Persona != nil {
				r.Persona = a.Persona
			}

			out = append(out, r)
		}
	}
	return out
}

// Summary returns a human-readable summary of what the team spec defines.
func (ts *TeamSpec) Summary() string {
	agents := ts.Resolve()
	var b strings.Builder
	fmt.Fprintf(&b, "Team: %s\n", ts.Metadata.Name)
	if len(ts.Metadata.Labels) > 0 {
		fmt.Fprintf(&b, "Labels: %v\n", ts.Metadata.Labels)
	}
	fmt.Fprintf(&b, "Agents: %d definitions → %d VMs\n\n", len(ts.Spec.Agents), len(agents))
	for _, a := range agents {
		fmt.Fprintf(&b, "  %-40s %s vcpu=%d ram=%s disk=%s mode=%s\n",
			a.VMName, a.Type, a.VCPU, a.RAM, a.Disk, a.Mode)
		if a.Persona != nil && a.Persona.Role != "" {
			fmt.Fprintf(&b, "    persona: %s\n", a.Persona.Role)
		}
		if len(a.Repos) > 0 {
			fmt.Fprintf(&b, "    repos: %s\n", strings.Join(a.Repos, ", "))
		}
		if len(a.Access) > 0 {
			fmt.Fprintf(&b, "    access: %s\n", strings.Join(a.Access, ", "))
		}
	}
	return b.String()
}
