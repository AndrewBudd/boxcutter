package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/AndrewBudd/boxcutter/vmid/internal/registry"
	"github.com/AndrewBudd/boxcutter/vmid/internal/token"
)

type AdminHandler struct {
	reg    *registry.Registry
	github *token.GitHubTokenMinter
}

func NewAdminHandler(reg *registry.Registry, github *token.GitHubTokenMinter) *AdminHandler {
	return &AdminHandler{reg: reg, github: github}
}

func (h *AdminHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /internal/vms", h.handleRegister)
	mux.HandleFunc("DELETE /internal/vms/{id}", h.handleDeregister)
	mux.HandleFunc("GET /internal/vms", h.handleList)
	mux.HandleFunc("GET /internal/vms/{id}", h.handleGet)
	mux.HandleFunc("POST /internal/vms/{id}/github-token", h.handleMintGitHubToken)
}

type registerRequest struct {
	VMID       string            `json:"vm_id"`
	IP         string            `json:"ip"`
	Labels     map[string]string `json:"labels,omitempty"`
	GitHubRepo string            `json:"github_repo,omitempty"`
}

func (h *AdminHandler) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.VMID == "" || req.IP == "" {
		http.Error(w, "vm_id and ip are required", http.StatusBadRequest)
		return
	}

	rec := &registry.VMRecord{
		VMID:       req.VMID,
		IP:         req.IP,
		Labels:     req.Labels,
		GitHubRepo: req.GitHubRepo,
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

func extractPathID(path, prefix string) string {
	s := strings.TrimPrefix(path, prefix)
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i]
	}
	return s
}
