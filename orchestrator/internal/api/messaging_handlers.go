package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
)

// Messaging API handlers for SNS/SQS-style topics and queues (#190).

func (h *Handler) registerMessagingRoutes(mux *http.ServeMux) {
	// Topics
	mux.HandleFunc("POST /api/topics", h.handleCreateTopic)
	mux.HandleFunc("GET /api/topics", h.handleListTopics)
	mux.HandleFunc("DELETE /api/topics/{name}", h.handleDeleteTopic)
	mux.HandleFunc("POST /api/topics/{name}/publish", h.handlePublish)

	// Queues
	mux.HandleFunc("GET /api/queues", h.handleListQueues)
	mux.HandleFunc("POST /api/queues", h.handleCreateQueue)
	mux.HandleFunc("GET /api/queues/{name}/messages", h.handleReceive)
	mux.HandleFunc("POST /api/queues/{name}/ack", h.handleAck)

	// Subscriptions
	mux.HandleFunc("POST /api/subscriptions", h.handleSubscribe)
	mux.HandleFunc("GET /api/subscriptions", h.handleListSubscriptions)
	mux.HandleFunc("DELETE /api/subscriptions/{id}", h.handleUnsubscribe)

	// Convenience
	mux.HandleFunc("POST /api/send/{agent}", h.handleSendToAgent)
}

func (h *Handler) handleCreateTopic(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if err := h.db.CreateTopic(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"name": req.Name, "status": "created"})
}

func (h *Handler) handleListTopics(w http.ResponseWriter, r *http.Request) {
	topics, err := h.db.ListTopics()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, topics)
}

func (h *Handler) handleDeleteTopic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.db.DeleteTopic(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handlePublish(w http.ResponseWriter, r *http.Request) {
	topicName := r.PathValue("name")
	var req struct {
		Body     string `json:"body"`
		From     string `json:"from"`
		Priority string `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Body == "" {
		http.Error(w, "body required", http.StatusBadRequest)
		return
	}
	count, err := h.db.Publish(topicName, req.Body, req.From, req.Priority)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{"delivered_to": count})
}

func (h *Handler) handleCreateQueue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if err := h.db.CreateQueue(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"name": req.Name, "status": "created"})
}

func (h *Handler) handleListQueues(w http.ResponseWriter, r *http.Request) {
	queues, err := h.db.ListQueues()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, queues)
}

func (h *Handler) handleReceive(w http.ResponseWriter, r *http.Request) {
	queueName := r.PathValue("name")
	msgs, err := h.db.Receive(queueName, 10)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if msgs == nil {
		msgs = []db.Message{}
	}
	writeJSON(w, msgs)
}

func (h *Handler) handleAck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MessageIDs []string `json:"message_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.MessageIDs) == 0 {
		http.Error(w, "message_ids required", http.StatusBadRequest)
		return
	}
	if err := h.db.Ack(req.MessageIDs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TopicName string `json:"topic_name"`
		QueueName string `json:"queue_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TopicName == "" || req.QueueName == "" {
		http.Error(w, "topic_name and queue_name required", http.StatusBadRequest)
		return
	}
	id, err := h.db.Subscribe(req.TopicName, req.QueueName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]interface{}{"id": id})
}

func (h *Handler) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	subs, err := h.db.ListSubscriptions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, subs)
}

func (h *Handler) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	var id int64
	fmt.Sscanf(idStr, "%d", &id)
	if err := h.db.Unsubscribe(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleSendToAgent(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	queueName := agent + ".inbox"

	var req struct {
		Body     string `json:"body"`
		From     string `json:"from"`
		Priority string `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Body == "" {
		http.Error(w, "body required", http.StatusBadRequest)
		return
	}

	// Auto-create queue if it doesn't exist
	h.db.CreateQueue(queueName)

	id, err := h.db.SendDirect(queueName, req.Body, req.From, req.Priority)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"id": id, "queue": queueName})
}
