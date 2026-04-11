package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/config"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/middleware"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/registry"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/sentinel"
)

// serveWithIdentity wraps a handler through the Identity middleware, injecting
// the VM record from a fwmark set via WithMark. This mirrors the real request
// path: fwmark → middleware lookup → VMFromContext in handler.
func serveWithIdentity(reg *registry.Registry, handler http.HandlerFunc, mark int) *httptest.ResponseRecorder {
	identityMW := middleware.Identity(reg)
	wrapped := identityMW(http.HandlerFunc(handler))

	req := httptest.NewRequest("GET", "/metadata/agent-config", nil)
	ctx := middleware.WithMark(req.Context(), mark)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	return w
}

// TestCredentialProvisioningFlow tests the full credential provisioning pipeline:
// 1. Admin API sets agent-config on a VM
// 2. Metadata endpoint serves credentials to the VM
// 3. Agent-config endpoint returns the VM's configuration
//
// This validates the path: orchestrator → admin API → registry → metadata → tapegun
func TestCredentialProvisioningFlow(t *testing.T) {
	// Setup: credential file on disk (simulates host secrets bundle)
	dir := t.TempDir()
	credPath := filepath.Join(dir, "claude-credentials.json")
	credJSON := `{"oauth_refresh_token":"refresh-tok-abc123","oauth_client_id":"client-id-xyz"}`
	if err := os.WriteFile(credPath, []byte(credJSON), 0600); err != nil {
		t.Fatal(err)
	}

	reg := registry.New()
	metaH := &MetadataHandler{
		sentinel: sentinel.NewStore(),
		reg:      reg,
		metadata: config.MetadataFilesConfig{
			ClaudeCredentialsPath: credPath,
		},
	}
	adminH := &AdminHandler{reg: reg}

	// Step 1: Register VM (simulates node agent registering a Firecracker VM)
	reg.Register(&registry.VMRecord{
		VMID: "vm-dev-1",
		IP:   "10.0.0.2",
		Mark: 100,
		Mode: "normal",
	})

	// Step 2: Set agent-config via admin API (simulates orchestrator's team apply)
	agentCfgJSON := `{
		"team": "platform",
		"agent": "developer",
		"persona": {
			"role": "developer",
			"claude_md": ".claude/personas/developer.md",
			"instructions": "Focus on backend Go services."
		},
		"repos": ["AndrewBudd/boxcutter"],
		"authorized": true
	}`
	req := httptest.NewRequest("PUT", "/internal/vms/vm-dev-1/agent-config", strings.NewReader(agentCfgJSON))
	req.SetPathValue("id", "vm-dev-1")
	w := httptest.NewRecorder()
	adminH.handleSetAgentConfig(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("SetAgentConfig: status = %d, want 204; body = %s", w.Code, w.Body.String())
	}

	// Step 3: VM fetches credentials from metadata endpoint (no identity required)
	req = httptest.NewRequest("GET", "/metadata/claude-credentials", nil)
	w = httptest.NewRecorder()
	metaH.handleClaudeCredentials(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("ClaudeCredentials: status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := w.Body.String(); body != credJSON {
		t.Errorf("credential body = %q, want %q", body, credJSON)
	}

	// Step 4: VM fetches agent-config (identity required — uses fwmark middleware)
	w = serveWithIdentity(reg, metaH.handleAgentConfig, 100)

	if w.Code != http.StatusOK {
		t.Fatalf("AgentConfig: status = %d, want 200", w.Code)
	}

	var agentCfg registry.AgentConfig
	if err := json.NewDecoder(w.Body).Decode(&agentCfg); err != nil {
		t.Fatalf("decode agent-config: %v", err)
	}
	if agentCfg.Team != "platform" {
		t.Errorf("Team = %q, want %q", agentCfg.Team, "platform")
	}
	if agentCfg.Agent != "developer" {
		t.Errorf("Agent = %q, want %q", agentCfg.Agent, "developer")
	}
	if agentCfg.Persona == nil || agentCfg.Persona.Role != "developer" {
		t.Errorf("Persona.Role = %v, want %q", agentCfg.Persona, "developer")
	}
	if len(agentCfg.Repos) != 1 || agentCfg.Repos[0] != "AndrewBudd/boxcutter" {
		t.Errorf("Repos = %v, want [AndrewBudd/boxcutter]", agentCfg.Repos)
	}
	if !agentCfg.Authorized {
		t.Error("Authorized = false, want true")
	}
}

// TestCredentialJSON_MatchesClaudeCodeFormat verifies the credential JSON structure
// matches what Claude Code expects in ~/.claude/.credentials.json.
func TestCredentialJSON_MatchesClaudeCodeFormat(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")

	// This is the format Claude Code writes after OAuth flow.
	// The metadata endpoint must serve this exact shape.
	creds := map[string]interface{}{
		"oauth_refresh_token": "rt_test_token_12345",
		"oauth_client_id":     "claude-client-id",
	}
	data, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(credPath, data, 0600)

	h := newTestMetadataHandler(t, config.MetadataFilesConfig{
		ClaudeCredentialsPath: credPath,
	})

	req := httptest.NewRequest("GET", "/metadata/claude-credentials", nil)
	w := httptest.NewRecorder()
	h.handleClaudeCredentials(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Parse response and verify required fields
	var got map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	if _, ok := got["oauth_refresh_token"]; !ok {
		t.Error("response missing oauth_refresh_token field")
	}
	if _, ok := got["oauth_client_id"]; !ok {
		t.Error("response missing oauth_client_id field")
	}
	if got["oauth_refresh_token"] != "rt_test_token_12345" {
		t.Errorf("oauth_refresh_token = %v, want %q", got["oauth_refresh_token"], "rt_test_token_12345")
	}
}

// TestCredentialRotation verifies that updating the credentials file on disk
// is immediately reflected in subsequent metadata requests (no caching).
func TestCredentialRotation(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	os.WriteFile(credPath, []byte(`{"oauth_refresh_token":"token-v1"}`), 0600)

	h := newTestMetadataHandler(t, config.MetadataFilesConfig{
		ClaudeCredentialsPath: credPath,
	})

	// First request: original token
	req := httptest.NewRequest("GET", "/metadata/claude-credentials", nil)
	w := httptest.NewRecorder()
	h.handleClaudeCredentials(w, req)

	var v1 map[string]string
	json.Unmarshal(w.Body.Bytes(), &v1)
	if v1["oauth_refresh_token"] != "token-v1" {
		t.Fatalf("initial token = %q, want token-v1", v1["oauth_refresh_token"])
	}

	// Rotate credentials on disk
	os.WriteFile(credPath, []byte(`{"oauth_refresh_token":"token-v2"}`), 0600)

	// Second request: rotated token
	req = httptest.NewRequest("GET", "/metadata/claude-credentials", nil)
	w = httptest.NewRecorder()
	h.handleClaudeCredentials(w, req)

	var v2 map[string]string
	json.Unmarshal(w.Body.Bytes(), &v2)
	if v2["oauth_refresh_token"] != "token-v2" {
		t.Errorf("rotated token = %q, want token-v2", v2["oauth_refresh_token"])
	}
}

// TestAgentConfig_MetadataEndpoint_NoConfig verifies that VMs without agent-config
// get an empty config (graceful degradation, not 404).
func TestAgentConfig_MetadataEndpoint_NoConfig(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.VMRecord{VMID: "vm-no-config", IP: "10.0.0.2", Mark: 300})

	h := &MetadataHandler{
		sentinel: sentinel.NewStore(),
		reg:      reg,
		metadata: config.MetadataFilesConfig{},
	}

	w := serveWithIdentity(reg, h.handleAgentConfig, 300)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty config, not 404)", w.Code)
	}

	var cfg registry.AgentConfig
	if err := json.NewDecoder(w.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Team != "" || cfg.Agent != "" {
		t.Errorf("expected empty config, got team=%q agent=%q", cfg.Team, cfg.Agent)
	}
}

// TestAgentConfig_PerVMIsolation verifies that each VM gets only its own
// agent-config, not another VM's. This validates fwmark-based identity isolation.
func TestAgentConfig_PerVMIsolation(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.VMRecord{VMID: "vm-dev", IP: "10.0.0.2", Mark: 100})
	reg.Register(&registry.VMRecord{VMID: "vm-qa", IP: "10.0.0.2", Mark: 200})

	// Set different agent-configs
	reg.SetAgentConfig("vm-dev", &registry.AgentConfig{
		Team:  "platform",
		Agent: "developer",
		Persona: &registry.PersonaConfig{
			Role:         "developer",
			Instructions: "Write code.",
		},
	})
	reg.SetAgentConfig("vm-qa", &registry.AgentConfig{
		Team:  "platform",
		Agent: "qa-engineer",
		Persona: &registry.PersonaConfig{
			Role:         "qa",
			Instructions: "Test code.",
		},
	})

	h := &MetadataHandler{
		sentinel: sentinel.NewStore(),
		reg:      reg,
		metadata: config.MetadataFilesConfig{},
	}

	// VM-dev fetches its config (fwmark=100)
	w := serveWithIdentity(reg, h.handleAgentConfig, 100)
	var devCfg registry.AgentConfig
	json.NewDecoder(w.Body).Decode(&devCfg)
	if devCfg.Agent != "developer" {
		t.Errorf("vm-dev got agent=%q, want developer", devCfg.Agent)
	}
	if devCfg.Persona.Instructions != "Write code." {
		t.Errorf("vm-dev instructions = %q", devCfg.Persona.Instructions)
	}

	// VM-qa fetches its config (fwmark=200)
	w = serveWithIdentity(reg, h.handleAgentConfig, 200)
	var qaCfg registry.AgentConfig
	json.NewDecoder(w.Body).Decode(&qaCfg)
	if qaCfg.Agent != "qa-engineer" {
		t.Errorf("vm-qa got agent=%q, want qa-engineer", qaCfg.Agent)
	}
	if qaCfg.Persona.Instructions != "Test code." {
		t.Errorf("vm-qa instructions = %q", qaCfg.Persona.Instructions)
	}
}

// TestAdminSetAgentConfig_InvalidJSON verifies the admin endpoint rejects malformed JSON.
func TestAdminSetAgentConfig_InvalidJSON(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})
	adminH := &AdminHandler{reg: reg}

	req := httptest.NewRequest("PUT", "/internal/vms/vm-1/agent-config", strings.NewReader("not json"))
	req.SetPathValue("id", "vm-1")
	w := httptest.NewRecorder()
	adminH.handleSetAgentConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestAdminSetAgentConfig_VMNotFound verifies 404 when setting config for unknown VM.
func TestAdminSetAgentConfig_VMNotFound(t *testing.T) {
	reg := registry.New()
	adminH := &AdminHandler{reg: reg}

	req := httptest.NewRequest("PUT", "/internal/vms/nonexistent/agent-config",
		strings.NewReader(`{"team":"x"}`))
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()
	adminH.handleSetAgentConfig(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestAdminGetAgentConfig_EmptyDefault verifies GET returns empty config for VM with no config set.
func TestAdminGetAgentConfig_EmptyDefault(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})
	adminH := &AdminHandler{reg: reg}

	req := httptest.NewRequest("GET", "/internal/vms/vm-1/agent-config", nil)
	req.SetPathValue("id", "vm-1")
	w := httptest.NewRecorder()
	adminH.handleGetAgentConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var cfg registry.AgentConfig
	json.NewDecoder(w.Body).Decode(&cfg)
	if cfg.Team != "" {
		t.Errorf("expected empty team, got %q", cfg.Team)
	}
}

// TestCredentialEndpoint_NotBehindIdentity verifies that the claude-credentials
// endpoint does NOT require VM identity (fwmark). It serves the same credentials
// to any requester — same trust model as SSH keys endpoint.
func TestCredentialEndpoint_NotBehindIdentity(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	os.WriteFile(credPath, []byte(`{"oauth_refresh_token":"shared-token"}`), 0600)

	h := newTestMetadataHandler(t, config.MetadataFilesConfig{
		ClaudeCredentialsPath: credPath,
	})

	// Request without any VM context — should still succeed
	req := httptest.NewRequest("GET", "/metadata/claude-credentials", nil)
	w := httptest.NewRecorder()
	h.handleClaudeCredentials(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no identity required)", w.Code)
	}
	var creds map[string]string
	json.Unmarshal(w.Body.Bytes(), &creds)
	if creds["oauth_refresh_token"] != "shared-token" {
		t.Errorf("token = %q, want shared-token", creds["oauth_refresh_token"])
	}
}

// TestAgentConfig_WithFullPersona verifies all persona fields round-trip through
// the admin API and metadata endpoint correctly.
func TestAgentConfig_WithFullPersona(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.VMRecord{VMID: "vm-persona", IP: "10.0.0.2", Mark: 150})

	adminH := &AdminHandler{reg: reg}
	metaH := &MetadataHandler{
		sentinel: sentinel.NewStore(),
		reg:      reg,
		metadata: config.MetadataFilesConfig{},
	}

	// Set agent-config with detailed persona
	cfgJSON := `{
		"team": "core",
		"agent": "reviewer",
		"replica_index": 2,
		"persona": {
			"role": "code-reviewer",
			"claude_md": ".claude/personas/reviewer.md",
			"instructions": "Review all PRs for security issues. Never approve without running tests."
		},
		"repos": ["AndrewBudd/boxcutter", "AndrewBudd/infra"],
		"tapegun": ["review-setup"],
		"authorized": true,
		"labels": {"env": "staging", "role": "reviewer"},
		"team_persona_files": [".claude/personas/developer.md", ".claude/personas/reviewer.md"]
	}`
	req := httptest.NewRequest("PUT", "/internal/vms/vm-persona/agent-config", strings.NewReader(cfgJSON))
	req.SetPathValue("id", "vm-persona")
	w := httptest.NewRecorder()
	adminH.handleSetAgentConfig(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("SetAgentConfig: status = %d, want 204", w.Code)
	}

	// Fetch via metadata endpoint (with identity)
	w = serveWithIdentity(reg, metaH.handleAgentConfig, 150)

	var cfg registry.AgentConfig
	if err := json.NewDecoder(w.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if cfg.ReplicaIndex != 2 {
		t.Errorf("ReplicaIndex = %d, want 2", cfg.ReplicaIndex)
	}
	if cfg.Persona == nil {
		t.Fatal("Persona is nil")
	}
	if cfg.Persona.Role != "code-reviewer" {
		t.Errorf("Persona.Role = %q, want code-reviewer", cfg.Persona.Role)
	}
	if cfg.Persona.ClaudeMD != ".claude/personas/reviewer.md" {
		t.Errorf("Persona.ClaudeMD = %q", cfg.Persona.ClaudeMD)
	}
	if !strings.Contains(cfg.Persona.Instructions, "security issues") {
		t.Errorf("Persona.Instructions missing security reference: %q", cfg.Persona.Instructions)
	}
	if len(cfg.Repos) != 2 {
		t.Errorf("Repos count = %d, want 2", len(cfg.Repos))
	}
	if len(cfg.Tapegun) != 1 || cfg.Tapegun[0] != "review-setup" {
		t.Errorf("Tapegun = %v, want [review-setup]", cfg.Tapegun)
	}
	if cfg.Labels["role"] != "reviewer" {
		t.Errorf("Labels[role] = %q, want reviewer", cfg.Labels["role"])
	}
	if len(cfg.TeamPersonaFiles) != 2 {
		t.Errorf("TeamPersonaFiles count = %d, want 2", len(cfg.TeamPersonaFiles))
	}
}

// TestAgentConfig_Overwrite verifies that setting agent-config twice replaces
// the previous config (not merges).
func TestAgentConfig_Overwrite(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})
	adminH := &AdminHandler{reg: reg}

	// First config
	req := httptest.NewRequest("PUT", "/internal/vms/vm-1/agent-config",
		strings.NewReader(`{"team":"alpha","agent":"dev","repos":["org/repo1","org/repo2"]}`))
	req.SetPathValue("id", "vm-1")
	w := httptest.NewRecorder()
	adminH.handleSetAgentConfig(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("first set: status = %d", w.Code)
	}

	// Overwrite with different config
	req = httptest.NewRequest("PUT", "/internal/vms/vm-1/agent-config",
		strings.NewReader(`{"team":"beta","agent":"qa"}`))
	req.SetPathValue("id", "vm-1")
	w = httptest.NewRecorder()
	adminH.handleSetAgentConfig(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("overwrite: status = %d", w.Code)
	}

	// Verify overwritten
	cfg, ok := reg.GetAgentConfig("vm-1")
	if !ok || cfg == nil {
		t.Fatal("GetAgentConfig failed after overwrite")
	}
	if cfg.Team != "beta" {
		t.Errorf("Team = %q, want beta (overwrite)", cfg.Team)
	}
	if cfg.Agent != "qa" {
		t.Errorf("Agent = %q, want qa (overwrite)", cfg.Agent)
	}
	if len(cfg.Repos) != 0 {
		t.Errorf("Repos = %v, want empty (full replace, not merge)", cfg.Repos)
	}
}

// TestAgentConfig_UnknownMark verifies that a request with an unregistered fwmark
// is rejected by the identity middleware (403 Forbidden).
func TestAgentConfig_UnknownMark(t *testing.T) {
	reg := registry.New()
	h := &MetadataHandler{
		sentinel: sentinel.NewStore(),
		reg:      reg,
		metadata: config.MetadataFilesConfig{},
	}

	w := serveWithIdentity(reg, h.handleAgentConfig, 9999)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for unknown fwmark", w.Code)
	}
}
