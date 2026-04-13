package ssh

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (h *Handler) cmdQueue(args []string) int {
	if len(args) == 0 {
		fmt.Println("Usage: ssh <host> queue <list|add|reassign|complete|sync|stats>")
		return 1
	}

	switch args[0] {
	case "list":
		return h.cmdQueueList(args[1:])
	case "add":
		return h.cmdQueueAdd(args[1:])
	case "reassign":
		if len(args) < 2 {
			fmt.Println("Usage: ssh <host> queue reassign <task-id>")
			return 1
		}
		return h.cmdQueueReassign(args[1])
	case "complete":
		if len(args) < 2 {
			fmt.Println("Usage: ssh <host> queue complete <task-id>")
			return 1
		}
		return h.cmdQueueComplete(args[1])
	case "sync":
		return h.cmdQueueSync()
	case "stats":
		return h.cmdQueueStats()
	default:
		fmt.Printf("Unknown queue command: %s\n", args[0])
		return 1
	}
}

func (h *Handler) cmdQueueList(args []string) int {
	path := "/api/queue"
	for i := 0; i < len(args); i++ {
		if args[i] == "--status" && i+1 < len(args) {
			path += "?status=" + args[i+1]
			i++
		}
	}

	body, err := h.get(path)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}

	var items []struct {
		ID         string  `json:"id"`
		SourceRef  string  `json:"source_ref"`
		Title      string  `json:"title"`
		Priority   int     `json:"priority"`
		Status     string  `json:"status"`
		AssignedTo string  `json:"assigned_to"`
		AssignedAt *string `json:"assigned_at"`
	}
	json.Unmarshal(body, &items)

	if len(items) == 0 {
		fmt.Println("  Queue is empty")
		return 0
	}

	for _, item := range items {
		ref := item.SourceRef
		if ref == "" {
			ref = item.ID
		}
		statusIcon := "  "
		switch item.Status {
		case "assigned":
			statusIcon = "-> "
		case "completed":
			statusIcon = "OK "
		case "failed":
			statusIcon = "!! "
		}

		line := fmt.Sprintf("  %s%-25s [%s] %s", statusIcon, ref, queuePriorityLabel(item.Priority), item.Title)
		if item.Status == "assigned" && item.AssignedTo != "" {
			age := ""
			if item.AssignedAt != nil {
				if t, err := time.Parse(time.RFC3339, *item.AssignedAt); err == nil {
					age = fmt.Sprintf(" (%s ago)", time.Since(t).Round(time.Minute))
				}
			}
			line += fmt.Sprintf("  assigned -> %s%s", item.AssignedTo, age)
		}
		fmt.Println(line)
	}
	return 0
}

func (h *Handler) cmdQueueAdd(args []string) int {
	var title, sourceRef, teamName string
	priority := 2

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--title":
			if i+1 < len(args) {
				title = args[i+1]
				i++
			}
		case "--source":
			if i+1 < len(args) {
				sourceRef = args[i+1]
				i++
			}
		case "--priority":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &priority)
				i++
			}
		case "--team":
			if i+1 < len(args) {
				teamName = args[i+1]
				i++
			}
		default:
			if title == "" {
				title = strings.Join(args[i:], " ")
				i = len(args)
			}
		}
	}

	if title == "" {
		fmt.Println("Usage: ssh <host> queue add --title \"Fix bug\" [--source owner/repo#42] [--priority 1] [--team name]")
		return 1
	}

	body := map[string]interface{}{
		"title":      title,
		"source":     "manual",
		"source_ref": sourceRef,
		"priority":   priority,
		"team":       teamName,
	}
	bodyJSON, _ := json.Marshal(body)

	resp, err := h.post("/api/queue", bodyJSON)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}

	var result struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	json.Unmarshal(resp, &result)
	fmt.Printf("  Enqueued: %s — %s\n", result.ID, result.Title)
	return 0
}

func (h *Handler) cmdQueueReassign(id string) int {
	_, err := h.post("/api/queue/"+id+"/reassign", nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	fmt.Printf("  Reassigned %s back to queue\n", id)
	return 0
}

func (h *Handler) cmdQueueComplete(id string) int {
	_, err := h.post("/api/queue/"+id+"/complete", nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	fmt.Printf("  Completed %s\n", id)
	return 0
}

func (h *Handler) cmdQueueSync() int {
	resp, err := h.post("/api/queue/sync", nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var result struct {
		Synced int `json:"synced"`
	}
	json.Unmarshal(resp, &result)
	fmt.Printf("  Synced %d new issues\n", result.Synced)
	return 0
}

func (h *Handler) cmdQueueStats() int {
	body, err := h.get("/api/queue/stats")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var stats struct {
		Total     int `json:"total"`
		Queued    int `json:"queued"`
		Assigned  int `json:"assigned"`
		Completed int `json:"completed"`
		Failed    int `json:"failed"`
	}
	json.Unmarshal(body, &stats)

	fmt.Printf("  Queue Statistics\n")
	fmt.Printf("  Total:     %d\n", stats.Total)
	fmt.Printf("  Queued:    %d\n", stats.Queued)
	fmt.Printf("  Assigned:  %d\n", stats.Assigned)
	fmt.Printf("  Completed: %d\n", stats.Completed)
	fmt.Printf("  Failed:    %d\n", stats.Failed)
	return 0
}

func queuePriorityLabel(p int) string {
	switch {
	case p == 0:
		return "p0-critical"
	case p == 1:
		return "p1-high"
	case p == 2:
		return "p2-medium"
	default:
		return "p3-low"
	}
}
