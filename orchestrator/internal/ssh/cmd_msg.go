package ssh

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (h *Handler) cmdMsg(args []string) int {
	if len(args) == 0 {
		fmt.Print(`Messaging — topics, queues, and subscriptions

Commands:
  msg topics                       List all topics
  msg queues                       List all queues (with depth)
  msg publish <topic> <message>    Publish to a topic
  msg send <agent> <message>       Send to an agent's inbox
  msg read <queue>                 Read messages from a queue
  msg subscribe <topic> <queue>    Subscribe queue to topic
  msg subscriptions                List all subscriptions
  msg inspect <queue>              Show queue depth
  msg delete <queue>               Delete a queue

Usage: ssh <host> msg <command> [args]
`)
		return 0
	}

	sub := args[0]
	switch sub {
	case "topics":
		resp, err := h.get("/api/topics")
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var topics []map[string]interface{}
		json.Unmarshal(resp, &topics)
		if len(topics) == 0 {
			fmt.Println("No topics.")
			return 0
		}
		fmt.Printf("%-30s %s\n", "TOPIC", "CREATED")
		for _, t := range topics {
			name, _ := t["name"].(string)
			created, _ := t["created_at"].(string)
			fmt.Printf("%-30s %s\n", name, created)
		}
		return 0

	case "queues":
		resp, err := h.get("/api/queues")
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var queues []map[string]interface{}
		json.Unmarshal(resp, &queues)
		if len(queues) == 0 {
			fmt.Println("No queues.")
			return 0
		}
		fmt.Printf("%-30s %-8s %s\n", "QUEUE", "DEPTH", "CREATED")
		for _, q := range queues {
			name, _ := q["name"].(string)
			depth, _ := q["depth"].(float64)
			created, _ := q["created_at"].(string)
			fmt.Printf("%-30s %-8.0f %s\n", name, depth, created)
		}
		return 0

	case "publish":
		if len(args) < 3 {
			fmt.Println("Usage: ssh <host> msg publish <topic> <message>")
			return 1
		}
		topic := args[1]
		msg := strings.Join(args[2:], " ")
		resp, err := h.post("/api/topics/"+topic+"/publish", map[string]string{
			"body": msg, "from": "ssh-user",
		})
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var result map[string]interface{}
		json.Unmarshal(resp, &result)
		fmt.Printf("Published to %s (delivered to %.0f queues)\n", topic, result["delivered_to"])
		return 0

	case "send":
		if len(args) < 3 {
			fmt.Println("Usage: ssh <host> msg send <agent> <message>")
			return 1
		}
		agent := args[1]
		msg := strings.Join(args[2:], " ")
		resp, err := h.post("/api/send/"+agent, map[string]string{
			"body": msg, "from": "ssh-user",
		})
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var result map[string]string
		json.Unmarshal(resp, &result)
		fmt.Printf("Sent to %s (id: %s)\n", result["queue"], result["id"])
		return 0

	case "read":
		if len(args) < 2 {
			fmt.Println("Usage: ssh <host> msg read <queue>")
			return 1
		}
		queue := args[1]
		resp, err := h.get("/api/queues/" + queue + "/messages")
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var msgs []map[string]interface{}
		json.Unmarshal(resp, &msgs)
		if len(msgs) == 0 {
			fmt.Println("No messages.")
			return 0
		}
		for _, m := range msgs {
			id, _ := m["id"].(string)
			from, _ := m["from_agent"].(string)
			body, _ := m["body"].(string)
			priority, _ := m["priority"].(string)
			fmt.Printf("[%s] from=%s priority=%s\n  %s\n\n", id, from, priority, body)
		}
		return 0

	case "subscribe":
		if len(args) < 3 {
			fmt.Println("Usage: ssh <host> msg subscribe <topic> <queue>")
			return 1
		}
		resp, err := h.post("/api/subscriptions", map[string]string{
			"topic_name": args[1], "queue_name": args[2],
		})
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var result map[string]interface{}
		json.Unmarshal(resp, &result)
		fmt.Printf("Subscribed %s → %s (id: %.0f)\n", args[1], args[2], result["id"])
		return 0

	case "subscriptions":
		resp, err := h.get("/api/subscriptions")
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var subs []map[string]interface{}
		json.Unmarshal(resp, &subs)
		if len(subs) == 0 {
			fmt.Println("No subscriptions.")
			return 0
		}
		fmt.Printf("%-6s %-25s → %s\n", "ID", "TOPIC", "QUEUE")
		for _, s := range subs {
			id, _ := s["id"].(float64)
			topic, _ := s["topic_name"].(string)
			queue, _ := s["queue_name"].(string)
			fmt.Printf("%-6.0f %-25s → %s\n", id, topic, queue)
		}
		return 0

	case "inspect":
		if len(args) < 2 {
			fmt.Println("Usage: ssh <host> msg inspect <queue>")
			return 1
		}
		resp, err := h.get("/api/queues")
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var queues []map[string]interface{}
		json.Unmarshal(resp, &queues)
		queue := args[1]
		for _, q := range queues {
			if q["name"] == queue {
				depth, _ := q["depth"].(float64)
				fmt.Printf("Queue: %s\nDepth: %.0f unacked messages\n", queue, depth)
				return 0
			}
		}
		fmt.Printf("Queue '%s' not found.\n", queue)
		return 1

	case "delete":
		if len(args) < 2 {
			fmt.Println("Usage: ssh <host> msg delete <queue>")
			return 1
		}
		_, err := h.delete("/api/queues/" + args[1])
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		fmt.Printf("Queue '%s' deleted.\n", args[1])
		return 0

	default:
		fmt.Printf("Unknown msg command: %s\n", sub)
		return 1
	}
}

// --- HTTP helpers ---

// postStream sends a POST and reads NDJSON progress events.
// Calls onProgress for each intermediate event.
// Returns the final "ready" event as a map.
func (h *Handler) postStream(path string, data interface{}, onProgress func(map[string]interface{})) (map[string]interface{}, error) {
	var body io.Reader
	if data != nil {
		b, _ := json.Marshal(data)
		body = strings.NewReader(string(b))
	}
	resp, err := http.Post(h.apiBase+path, "application/json", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(errBody)))
	}

	decoder := json.NewDecoder(resp.Body)
	var last map[string]interface{}
	for decoder.More() {
		var evt map[string]interface{}
		if err := decoder.Decode(&evt); err != nil {
			break
		}
		phase, _ := evt["phase"].(string)
		if phase == "error" {
			errMsg, _ := evt["error"].(string)
			return nil, fmt.Errorf("%s", errMsg)
		}
		if phase == "ready" {
			last = evt
		} else {
			if onProgress != nil {
				onProgress(evt)
			}
		}
	}
	if last == nil {
		return nil, fmt.Errorf("no response from server")
	}
	return last, nil
}

func (h *Handler) get(path string) ([]byte, error) {
	resp, err := http.Get(h.apiBase + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (h *Handler) post(path string, data interface{}) ([]byte, error) {
	var body io.Reader
	if data != nil {
		b, _ := json.Marshal(data)
		body = strings.NewReader(string(b))
	}
	resp, err := http.Post(h.apiBase+path, "application/json", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

