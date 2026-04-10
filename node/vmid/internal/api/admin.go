package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/registry"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/sentinel"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/token"
)

type AdminHandler struct {
	reg      *registry.Registry
	github   *token.GitHubTokenMinter
	sentinel *sentinel.Store
}

func NewAdminHandler(reg *registry.Registry, github *token.GitHubTokenMinter, sentinel *sentinel.Store) *AdminHandler {
	return &AdminHandler{reg: reg, github: github, sentinel: sentinel}
}

func (h *AdminHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /internal/vms", h.handleRegister)
	mux.HandleFunc("DELETE /internal/vms/{id}", h.handleDeregister)
	mux.HandleFunc("GET /internal/vms", h.handleList)
	mux.HandleFunc("GET /internal/vms/{id}", h.handleGet)
	mux.HandleFunc("POST /internal/vms/{id}/github-token", h.handleMintGitHubToken)
	mux.HandleFunc("POST /internal/vms/{id}/repos", h.handleAddRepo)
	mux.HandleFunc("DELETE /internal/vms/{id}/repos/{repo...}", h.handleRemoveRepo)
	mux.HandleFunc("GET /internal/vms/{id}/repos", h.handleListRepos)
	mux.HandleFunc("POST /internal/vms/{id}/projects", h.handleAddProject)
	mux.HandleFunc("DELETE /internal/vms/{id}/projects/{project...}", h.handleRemoveProject)
	mux.HandleFunc("GET /internal/vms/{id}/projects", h.handleListProjects)
	mux.HandleFunc("POST /internal/ghcr-token", h.handleGHCRToken)
	mux.HandleFunc("GET /internal/ghcr-token", h.handleGHCRToken)
	mux.HandleFunc("GET /internal/sentinel/{sentinel}", h.handleSentinelSwap)

	// Tapegun endpoints
	mux.HandleFunc("GET /internal/vms/{id}/activity", h.handleGetActivity)
	mux.HandleFunc("GET /internal/vms/{id}/health", h.handleGetHealth)
	mux.HandleFunc("POST /internal/vms/{id}/inbox", h.handlePostInbox)
	mux.HandleFunc("GET /internal/vms/{id}/inbox", h.handleGetInbox)
	mux.HandleFunc("GET /internal/tapegun/activity", h.handleAllActivity)

	// Agent config
	mux.HandleFunc("PUT /internal/vms/{id}/agent-config", h.handleSetAgentConfig)
	mux.HandleFunc("GET /internal/vms/{id}/agent-config", h.handleGetAgentConfig)

	// VM-to-VM mailbox (used by node agent for relay + migration)
	mux.HandleFunc("POST /internal/vms/{id}/mailbox", h.handlePushMailbox)
	mux.HandleFunc("GET /internal/vms/{id}/mailbox", h.handleExportMailbox)

	// Channel events (pushed by node agent / orchestrator)
	mux.HandleFunc("POST /internal/vms/{id}/channel/send", h.handleChannelSend)
	mux.HandleFunc("GET /internal/vms/{id}/channel/replies", h.handleChannelReplies)
}

type registerRequest struct {
	VMID        string                `json:"vm_id"`
	VMType      string                `json:"vm_type,omitempty"`
	IP          string                `json:"ip"`
	Mark        int                   `json:"mark"`
	Mode        string                `json:"mode"`
	Labels      map[string]string     `json:"labels,omitempty"`
	GitHubRepo  string                `json:"github_repo,omitempty"`
	GitHubRepos []string              `json:"github_repos,omitempty"`
	AgentConfig *registry.AgentConfig `json:"agent_config,omitempty"`
}

func (h *AdminHandler) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.VMID == "" {
		http.Error(w, "vm_id is required", http.StatusBadRequest)
		return
	}
	if req.Mode == "" {
		req.Mode = "normal"
	}

	rec := &registry.VMRecord{
		VMID:        req.VMID,
		VMType:      req.VMType,
		IP:          req.IP,
		Mark:        req.Mark,
		Mode:        req.Mode,
		Labels:      req.Labels,
		GitHubRepo:  req.GitHubRepo,
		GitHubRepos: req.GitHubRepos,
		AgentConfig: req.AgentConfig,
	}

	// Merge labels from agent_config into VM labels
	if req.AgentConfig != nil && len(req.AgentConfig.Labels) > 0 {
		if rec.Labels == nil {
			rec.Labels = make(map[string]string)
		}
		for k, v := range req.AgentConfig.Labels {
			if _, exists := rec.Labels[k]; !exists {
				rec.Labels[k] = v
			}
		}
	}

	h.reg.Register(rec)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, rec)
}

func (h *AdminHandler) handleDeregister(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	if !h.reg.Deregister(id) {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	h.sentinel.PurgeVM(id)
	w.WriteHeader(http.StatusNoContent)
}

func (h *AdminHandler) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.reg.List())
}

func (h *AdminHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	rec, ok := h.reg.LookupID(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	writeJSON(w, rec)
}

func (h *AdminHandler) handleMintGitHubToken(w http.ResponseWriter, r *http.Request) {
	if h.github == nil {
		http.Error(w, "GitHub integration not configured", http.StatusNotFound)
		return
	}

	id := r.PathValue("id")
	if id == "" {
		// fallback: parse from path
		path := r.URL.Path
		path = strings.TrimPrefix(path, "/internal/vms/")
		path = strings.TrimSuffix(path, "/github-token")
		id = path
	}

	rec, ok := h.reg.LookupID(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}

	tok, err := h.github.MintToken(rec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, tok)
}

func (h *AdminHandler) handleAddRepo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	rec, ok := h.reg.LookupID(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	var req struct {
		Repo string `json:"repo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Repo == "" {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return
	}
	rec.AddRepo(req.Repo)
	writeJSON(w, map[string]interface{}{
		"repos": rec.AllGitHubRepos(),
	})
}

func (h *AdminHandler) handleRemoveRepo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	repo := r.PathValue("repo")
	rec, ok := h.reg.LookupID(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	if !rec.RemoveRepo(repo) {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]interface{}{
		"repos": rec.AllGitHubRepos(),
	})
}

func (h *AdminHandler) handleListRepos(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	rec, ok := h.reg.LookupID(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]interface{}{
		"repos": rec.AllGitHubRepos(),
	})
}

func (h *AdminHandler) handleAddProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	rec, ok := h.reg.LookupID(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	var req struct {
		Project string `json:"project"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Project == "" {
		http.Error(w, "project is required", http.StatusBadRequest)
		return
	}
	rec.AddProject(req.Project)
	writeJSON(w, map[string]interface{}{
		"projects": rec.GitHubProjects,
	})
}

func (h *AdminHandler) handleRemoveProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project := r.PathValue("project")
	rec, ok := h.reg.LookupID(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	if !rec.RemoveProject(project) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]interface{}{
		"projects": rec.GitHubProjects,
	})
}

func (h *AdminHandler) handleListProjects(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	rec, ok := h.reg.LookupID(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]interface{}{
		"projects": rec.GitHubProjects,
	})
}

func (h *AdminHandler) handleGHCRToken(w http.ResponseWriter, r *http.Request) {
	if h.github == nil {
		http.Error(w, "GitHub integration not configured", http.StatusNotFound)
		return
	}

	tok, err := h.github.MintPackagesToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, tok)
}

func (h *AdminHandler) handleSentinelSwap(w http.ResponseWriter, r *http.Request) {
	sv := r.PathValue("sentinel")
	if sv == "" {
		sv = extractPathID(r.URL.Path, "/internal/sentinel/")
	}
	real, ok := h.sentinel.Swap(sv)
	if !ok {
		http.Error(w, "sentinel not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"token": real})
}

func (h *AdminHandler) handleGetActivity(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	activity, ok := h.reg.GetActivity(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	writeJSON(w, activity)
}

func (h *AdminHandler) handleGetHealth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	health, ok := h.reg.GetHealth(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	writeJSON(w, health)
}

func (h *AdminHandler) handlePostInbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	var msg registry.Message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !h.reg.PushMessage(id, &msg) {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *AdminHandler) handleGetInbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	msgs, ok := h.reg.PeekInbox(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	if msgs == nil {
		msgs = []*registry.Message{}
	}
	writeJSON(w, msgs)
}

func (h *AdminHandler) handleAllActivity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.reg.AllActivity())
}

func (h *AdminHandler) handlePushMailbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	var msg registry.MailboxMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !h.reg.PushMailbox(id, &msg) {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *AdminHandler) handleExportMailbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	msgs, ok := h.reg.ExportMailbox(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	if msgs == nil {
		msgs = []*registry.MailboxMessage{}
	}
	writeJSON(w, msgs)
}

func (h *AdminHandler) handleSetAgentConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	var cfg registry.AgentConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !h.reg.SetAgentConfig(id, &cfg) {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *AdminHandler) handleGetAgentConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}
	cfg, ok := h.reg.GetAgentConfig(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	if cfg == nil {
		cfg = &registry.AgentConfig{}
	}
	writeJSON(w, cfg)
}

// handleChannelSend pushes a channel event to a VM's SSE subscribers.
func (h *AdminHandler) handleChannelSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}

	var evt registry.ChannelEvent
	if err := json.NewDecoder(r.Body).Decode(&evt); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if evt.Timestamp == "" {
		evt.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if evt.Source == "" {
		evt.Source = "boxcutter"
	}

	if !h.reg.PushChannelEvent(id, &evt) {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleChannelReplies returns and removes pending channel replies from a VM.
func (h *AdminHandler) handleChannelReplies(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = extractPathID(r.URL.Path, "/internal/vms/")
	}

	replies, ok := h.reg.PopChannelReplies(id)
	if !ok {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	if replies == nil {
		replies = []*registry.ChannelReply{}
	}
	writeJSON(w, replies)
}

func extractPathID(path, prefix string) string {
	s := strings.TrimPrefix(path, prefix)
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i]
	}
	return s
}
