package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/middleware"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/registry"
)

// MessagingHandler serves VM-to-VM messaging endpoints on the metadata service.
type MessagingHandler struct {
	reg          *registry.Registry
	nodeAgentURL string // e.g. "http://localhost:8800"
}

func NewMessagingHandler(reg *registry.Registry, nodeAgentURL string) *MessagingHandler {
	return &MessagingHandler{reg: reg, nodeAgentURL: nodeAgentURL}
}

func (h *MessagingHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /messages/send", h.handleSend)
	mux.HandleFunc("GET /messages", h.handleConsume)
	mux.HandleFunc("DELETE /messages/{id}", h.handleAck)
}

type sendRequest struct {
	To      string `json:"to"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
}

func (h *MessagingHandler) handleSend(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.To == "" || req.Body == "" {
		http.Error(w, "to and body are required", http.StatusBadRequest)
		return
	}

	msg := &registry.MailboxMessage{
		ID:        fmt.Sprintf("msg-%d", time.Now().UnixNano()),
		From:      rec.VMID,
		To:        req.To,
		Subject:   req.Subject,
		Body:      req.Body,
		CreatedAt: time.Now(),
	}

	// Try local delivery first
	if h.reg.PushMailbox(req.To, msg) {
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]string{"id": msg.ID, "status": "delivered"})
		return
	}

	// Not local — relay through node agent
	if h.nodeAgentURL == "" {
		http.Error(w, "target VM not found locally and no relay configured", http.StatusNotFound)
		return
	}

	body, _ := json.Marshal(msg)
	resp, err := http.Post(h.nodeAgentURL+"/api/messages/relay", "application/json", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "relay failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&errBody)
		if errBody.Error != "" {
			http.Error(w, errBody.Error, resp.StatusCode)
		} else {
			http.Error(w, fmt.Sprintf("relay returned %d", resp.StatusCode), resp.StatusCode)
		}
		return
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"id": msg.ID, "status": "relayed"})
}

func (h *MessagingHandler) handleConsume(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	msgs, _ := h.reg.ConsumeMailbox(rec.VMID)
	if msgs == nil {
		msgs = []*registry.MailboxMessage{}
	}
	writeJSON(w, msgs)
}

func (h *MessagingHandler) handleAck(w http.ResponseWriter, r *http.Request) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		http.Error(w, "no VM context", http.StatusInternalServerError)
		return
	}

	msgID := r.PathValue("id")
	if msgID == "" {
		http.Error(w, "message id required", http.StatusBadRequest)
		return
	}

	if !h.reg.AckMailbox(rec.VMID, msgID) {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
