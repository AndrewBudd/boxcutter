package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/middleware"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/registry"
)

// WingmanHandler serves VM-facing wingman endpoints (activity reporting, inbox).
type WingmanHandler struct {
	reg *registry.Registry
}

func NewWingmanHandler(reg *registry.Registry) *WingmanHandler {
	return &WingmanHandler{reg: reg}
}

func (h *WingmanHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /wingman/activity", h.handlePostActivity)
	mux.HandleFunc("GET /wingman/inbox", h.handleGetInbox)
	mux.HandleFunc("POST /wingman/inbox/ack", h.handleAckInbox)
}

func (h *WingmanHandler) handlePostActivity(w http.ResponseWriter, r *http.Request) {
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

func (h *WingmanHandler) handleGetInbox(w http.ResponseWriter, r *http.Request) {
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

func (h *WingmanHandler) handleAckInbox(w http.ResponseWriter, r *http.Request) {
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
