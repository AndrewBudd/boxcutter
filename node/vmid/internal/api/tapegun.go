package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/middleware"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/registry"
)

// TapegunHandler serves VM-facing tapegun endpoints (activity reporting, inbox, checkpoints).
type TapegunHandler struct {
	reg           *registry.Registry
	checkpointDir string // directory for checkpoint session files
}

func NewTapegunHandler(reg *registry.Registry) *TapegunHandler {
	cpDir := "/var/lib/vmid/checkpoints"
	os.MkdirAll(cpDir, 0755)
	return &TapegunHandler{reg: reg, checkpointDir: cpDir}
}

func (h *TapegunHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /tapegun/activity", h.handlePostActivity)
	mux.HandleFunc("POST /tapegun/status", h.handlePostStatus)
	mux.HandleFunc("POST /tapegun/health", h.handlePostHealth)
	mux.HandleFunc("GET /tapegun/inbox", h.handleGetInbox)
	mux.HandleFunc("POST /tapegun/inbox/ack", h.handleAckInbox)
	mux.HandleFunc("POST /tapegun/checkpoint", h.handlePostCheckpoint)
	mux.HandleFunc("GET /tapegun/checkpoint", h.handleGetCheckpoint)
	mux.HandleFunc("POST /tapegun/files", h.handlePostFiles)
	mux.HandleFunc("GET /tapegun/files", h.handleGetFiles)
}

func (h *TapegunHandler) handlePostActivity(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	var report registry.ActivityReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if report.Timestamp.IsZero() {
		report.Timestamp = time.Now()
	}

	h.reg.SetActivity(rec.VMID, &report)
	w.WriteHeader(http.StatusNoContent)
}

func (h *TapegunHandler) handlePostStatus(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	h.reg.SetStatus(rec.VMID, &registry.StatusReport{
		Timestamp: time.Now(),
		Status:    req.Status,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *TapegunHandler) handlePostHealth(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	var report registry.HealthReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if report.Timestamp.IsZero() {
		report.Timestamp = time.Now()
	}

	h.reg.SetHealth(rec.VMID, &report)
	w.WriteHeader(http.StatusNoContent)
}

func (h *TapegunHandler) handleGetInbox(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	msgs, _ := h.reg.PopUnread(rec.VMID)
	if msgs == nil {
		msgs = []*registry.Message{}
	}
	writeJSON(w, msgs)
}

func (h *TapegunHandler) handleAckInbox(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	var req struct {
		MessageIDs []string `json:"message_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	h.reg.AckMessages(rec.VMID, req.MessageIDs)
	w.WriteHeader(http.StatusNoContent)
}

const maxCheckpointSize = 20 * 1024 * 1024 // 20MB

// handlePostCheckpoint stores a session checkpoint (conversation JSONL + git state).
// The session data is stored as a file on disk; metadata is kept in the registry.
func (h *TapegunHandler) handlePostCheckpoint(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	// Read body with size limit
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCheckpointSize+1))
	if err != nil {
		http.Error(w, "read error: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxCheckpointSize {
		http.Error(w, "checkpoint too large (max 20MB)", http.StatusRequestEntityTooLarge)
		return
	}

	var req struct {
		SessionID   string `json:"session_id"`
		GitBranch   string `json:"git_branch"`
		GitStash    string `json:"git_stash"`
		SessionData string `json:"session_data"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}

	// Write session data to disk
	vmDir := filepath.Join(h.checkpointDir, rec.VMID)
	os.MkdirAll(vmDir, 0755)
	sessionFile := filepath.Join(vmDir, req.SessionID+".jsonl")
	if err := os.WriteFile(sessionFile, []byte(req.SessionData), 0644); err != nil {
		http.Error(w, "write error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Update registry metadata
	cp := &registry.CheckpointData{
		SessionID: req.SessionID,
		GitBranch: req.GitBranch,
		GitStash:  req.GitStash,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		SizeBytes: len(req.SessionData),
		FilePath:  sessionFile,
	}
	h.reg.SetCheckpoint(rec.VMID, cp)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]interface{}{
		"status":     "stored",
		"session_id": req.SessionID,
		"size_bytes": len(req.SessionData),
	})
}

// handleGetCheckpoint retrieves the latest checkpoint for the requesting VM.
func (h *TapegunHandler) handleGetCheckpoint(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	cp, _ := h.reg.GetCheckpoint(rec.VMID)
	if cp == nil || cp.FilePath == "" {
		writeJSON(w, map[string]interface{}{})
		return
	}

	// Read session data from disk
	data, err := os.ReadFile(cp.FilePath)
	if err != nil {
		// Metadata exists but file is gone — stale checkpoint
		writeJSON(w, map[string]interface{}{})
		return
	}

	writeJSON(w, map[string]interface{}{
		"session_id":   cp.SessionID,
		"git_branch":   cp.GitBranch,
		"git_stash":    cp.GitStash,
		"timestamp":    cp.Timestamp,
		"session_data": string(data),
		"size_bytes":   len(data),
	})
}

// HandleGetCheckpointByVM retrieves a checkpoint for a specific VM (admin API).
func (h *TapegunHandler) HandleGetCheckpointByVM(vmID string) (map[string]interface{}, error) {
	cp, ok := h.reg.GetCheckpoint(vmID)
	if !ok || cp == nil || cp.FilePath == "" {
		return nil, fmt.Errorf("no checkpoint for %s", vmID)
	}
	data, err := os.ReadFile(cp.FilePath)
	if err != nil {
		return nil, fmt.Errorf("checkpoint file missing: %w", err)
	}
	return map[string]interface{}{
		"session_id":   cp.SessionID,
		"git_branch":   cp.GitBranch,
		"git_stash":    cp.GitStash,
		"timestamp":    cp.Timestamp,
		"session_data": string(data),
		"size_bytes":   len(data),
	}, nil
}

func (h *TapegunHandler) handlePostFiles(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	var ft registry.FileTracking
	if err := json.NewDecoder(r.Body).Decode(&ft); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if ft.Timestamp == "" {
		ft.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	h.reg.SetFiles(rec.VMID, &ft)
	w.WriteHeader(http.StatusNoContent)
}

func (h *TapegunHandler) handleGetFiles(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	ft, _ := h.reg.GetFiles(rec.VMID)
	if ft == nil {
		ft = &registry.FileTracking{}
	}
	writeJSON(w, ft)
}
