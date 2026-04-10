package team

import (
	"fmt"
	"strconv"
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
// Agent-level values override defaults. List fields (repos, access, tapegun)
// are replaced, not merged.
type AgentDefaults struct {
	Type       string   `yaml:"type,omitempty"`
	RAM        string   `yaml:"ram,omitempty"`
	VCPU       int      `yaml:"vcpu,omitempty"`
	Disk       string   `yaml:"disk,omitempty"`
	Mode       string   `yaml:"mode,omitempty"`
	Repos      []string `yaml:"repos,omitempty"`
	Authorized bool     `yaml:"authorized,omitempty"`
	Persona    *Persona `yaml:"persona,omitempty"`
	Tapegun    []string `yaml:"tapegun,omitempty"`
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
	Tapegun     []string `yaml:"tapegun,omitempty"`
}

type Persona struct {
	Role         string `yaml:"role,omitempty"`
	ClaudeMD     string `yaml:"claude_md,omitempty"`
	Instructions string `yaml:"instructions,omitempty"`
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
	Tapegun     []string
}

// RAMMiB parses the human-readable RAM string (e.g. "4G", "2048M") to MiB.
func (r *ResolvedAgent) RAMMiB() int {
	return ParseRAMMiB(r.RAM)
}

// ParseRAMMiB converts a human-readable RAM string to MiB.
func ParseRAMMiB(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	s = strings.ToUpper(s)
	if strings.HasSuffix(s, "G") {
		if n, err := strconv.Atoi(s[:len(s)-1]); err == nil {
			return n * 1024
		}
	}
	if strings.HasSuffix(s, "M") {
		if n, err := strconv.Atoi(s[:len(s)-1]); err == nil {
			return n
		}
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return 0
}

// CloneURLs builds GitHub clone URLs from the Repos list.
func (r *ResolvedAgent) CloneURLs() []string {
	var urls []string
	for _, repo := range r.Repos {
		if strings.HasPrefix(repo, "http") || strings.HasPrefix(repo, "git@") {
			urls = append(urls, repo)
		} else {
			urls = append(urls, "https://github.com/"+repo+".git")
		}
	}
	return urls
}

// Labels returns the metadata labels for this VM.
func (r *ResolvedAgent) Labels(teamName string) map[string]string {
	labels := map[string]string{
		"team":          teamName,
		"agent":         r.AgentName,
		"replica_index": fmt.Sprintf("%d", r.ReplicaNum),
	}
	if r.Persona != nil && r.Persona.Role != "" {
		labels["role"] = r.Persona.Role
	}
	return labels
}

// AgentConfigJSON builds the agent-config JSON blob sent to the vmid metadata
// service. This is what the tapegun setup phase reads on boot to configure the
// workspace (persona, repos, etc.).
func (r *ResolvedAgent) AgentConfigJSON(teamName string) map[string]interface{} {
	cfg := map[string]interface{}{
		"team":  teamName,
		"agent": r.AgentName,
	}
	if r.ReplicaNum > 0 {
		cfg["replica_index"] = r.ReplicaNum
	}
	if r.Persona != nil {
		p := map[string]interface{}{}
		if r.Persona.Role != "" {
			p["role"] = r.Persona.Role
		}
		if r.Persona.ClaudeMD != "" {
			p["claude_md"] = r.Persona.ClaudeMD
		}
		if r.Persona.Instructions != "" {
			p["instructions"] = r.Persona.Instructions
		}
		cfg["persona"] = p
	}
	if len(r.Repos) > 0 {
		cfg["repos"] = r.Repos
	}
	if len(r.Access) > 0 {
		cfg["access"] = r.Access
	}
	if r.Authorized {
		cfg["authorized"] = true
	}
	if len(r.Tapegun) > 0 {
		cfg["tapegun"] = r.Tapegun
	}
	return cfg
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
				r.Tapegun = d.Tapegun
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
			if a.Tapegun != nil {
				r.Tapegun = a.Tapegun
			}

			out = append(out, r)
		}
	}
	return out
}
