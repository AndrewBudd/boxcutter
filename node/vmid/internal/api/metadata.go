package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/config"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/middleware"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/sentinel"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/token"
)

type MetadataHandler struct {
	jwt      *token.JWTIssuer
	github   *token.GitHubTokenMinter
	sentinel *sentinel.Store
	metadata config.MetadataFilesConfig
}

func NewMetadataHandler(jwt *token.JWTIssuer, github *token.GitHubTokenMinter, sentinel *sentinel.Store, metadata config.MetadataFilesConfig) *MetadataHandler {
	return &MetadataHandler{jwt: jwt, github: github, sentinel: sentinel, metadata: metadata}
}

func (h *MetadataHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /identity", h.handleIdentity)
	mux.HandleFunc("GET /token", h.handleToken)
	mux.HandleFunc("GET /token/github", h.handleGitHubToken)
	mux.HandleFunc("GET /.well-known/jwks.json", h.handleJWKS)
	mux.HandleFunc("GET /metadata/ssh-keys", h.handleSSHKeys)
	mux.HandleFunc("GET /metadata/ca-cert", h.handleCACert)
	// Metadata-style root
	mux.HandleFunc("GET /", h.handleRoot)
}

func (h *MetadataHandler) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{
		"vm_id":  rec.VMID,
		"ip":     rec.IP,
		"labels": rec.Labels,
		"endpoints": map[string]string{
			"identity": "/identity",
			"token":    "/token",
			"github":   "/token/github",
			"jwks":     "/.well-known/jwks.json",
			"ssh_keys": "/metadata/ssh-keys",
			"ca_cert":  "/metadata/ca-cert",
		},
	})
}

func (h *MetadataHandler) handleIdentity(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}
	writeJSON(w, rec)
}

func (h *MetadataHandler) handleToken(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	audience := r.URL.Query().Get("audience")
	signed, exp, err := h.jwt.Mint(rec, audience)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]interface{}{
		"token":      signed,
		"expires_at": exp.Format("2006-01-02T15:04:05Z"),
		"type":       "Bearer",
	})
}

func (h *MetadataHandler) handleGitHubToken(w http.ResponseWriter, r *http.Request) {
	if h.github == nil {
		http.Error(w, "GitHub integration not configured", http.StatusNotFound)
		return
	}

	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	tok, err := h.github.MintToken(rec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Paranoid mode: wrap real token in a sentinel
	if rec.Mode == "paranoid" && tok.Token != "" {
		sv, err := h.sentinel.Put(rec.VMID, tok.Token, "github")
		if err != nil {
			http.Error(w, "sentinel error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		tok.Token = sv
	}

	writeJSON(w, tok)
}

func (h *MetadataHandler) handleJWKS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.jwt.JWKS())
}

// HandleJWKS is the exported version for use outside the identity middleware.
func (h *MetadataHandler) HandleJWKS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.jwt.JWKS())
}

// HandleSSHKeys is the exported version for use outside the identity middleware.
func (h *MetadataHandler) HandleSSHKeys(w http.ResponseWriter, r *http.Request) {
	h.handleSSHKeys(w, r)
}

// HandleCACert is the exported version for use outside the identity middleware.
func (h *MetadataHandler) HandleCACert(w http.ResponseWriter, r *http.Request) {
	h.handleCACert(w, r)
}

// handleSSHKeys returns the combined SSH authorized keys from all configured sources.
// This is NOT behind identity middleware — any VM on the network can fetch keys.
func (h *MetadataHandler) handleSSHKeys(w http.ResponseWriter, r *http.Request) {
	var keys []string
	for _, path := range h.metadata.SSHAuthorizedKeys {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line != "" {
				keys = append(keys, line)
			}
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(strings.Join(keys, "\n") + "\n"))
}

// handleCACert returns the internal CA certificate.
func (h *MetadataHandler) handleCACert(w http.ResponseWriter, r *http.Request) {
	if h.metadata.CACertPath == "" {
		http.Error(w, "no CA cert configured", http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(h.metadata.CACertPath)
	if err != nil {
		http.Error(w, "CA cert not available", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Write(data)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
