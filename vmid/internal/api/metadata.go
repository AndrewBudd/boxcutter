package api

import (
	"encoding/json"
	"net/http"

	"github.com/AndrewBudd/boxcutter/vmid/internal/middleware"
	"github.com/AndrewBudd/boxcutter/vmid/internal/sentinel"
	"github.com/AndrewBudd/boxcutter/vmid/internal/token"
)

type MetadataHandler struct {
	jwt      *token.JWTIssuer
	github   *token.GitHubTokenMinter
	sentinel *sentinel.Store
}

func NewMetadataHandler(jwt *token.JWTIssuer, github *token.GitHubTokenMinter, sentinel *sentinel.Store) *MetadataHandler {
	return &MetadataHandler{jwt: jwt, github: github, sentinel: sentinel}
}

func (h *MetadataHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /identity", h.handleIdentity)
	mux.HandleFunc("GET /token", h.handleToken)
	mux.HandleFunc("GET /token/github", h.handleGitHubToken)
	mux.HandleFunc("GET /.well-known/jwks.json", h.handleJWKS)
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

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
