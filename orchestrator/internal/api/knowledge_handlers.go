package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
)

// Knowledge API — persistent per-agent key/value store for team learnings.

func (h *Handler) registerKnowledgeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /api/knowledge/{agent}/{key}", h.handlePutKnowledge)
	mux.HandleFunc("GET /api/knowledge/{agent}/{key}", h.handleGetKnowledge)
	mux.HandleFunc("GET /api/knowledge/{agent}", h.handleListKnowledge)
	mux.HandleFunc("GET /api/knowledge", h.handleListAllKnowledge)
	mux.HandleFunc("DELETE /api/knowledge/{agent}/{key}", h.handleDeleteKnowledge)
}

func (h *Handler) handlePutKnowledge(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	key := r.PathValue("key")

	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := h.db.PutKnowledge(agent, key, body.Value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"agent": agent,
		"key":   key,
		"value": body.Value,
	})
}

func (h *Handler) handleGetKnowledge(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	key := r.PathValue("key")

	value, err := h.db.GetKnowledge(agent, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"agent": agent,
		"key":   key,
		"value": value,
	})
}

func (h *Handler) handleListKnowledge(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")

	entries, err := h.db.ListKnowledge(agent)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []db.KnowledgeEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

func (h *Handler) handleListAllKnowledge(w http.ResponseWriter, r *http.Request) {
	entries, err := h.db.ListAllKnowledge()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []db.KnowledgeEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

func (h *Handler) handleDeleteKnowledge(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	key := r.PathValue("key")

	if err := h.db.DeleteKnowledge(agent, key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
