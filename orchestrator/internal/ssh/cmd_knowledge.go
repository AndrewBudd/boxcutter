package ssh

import (
	"encoding/json"
	"fmt"
	"strings"
)

func (h *Handler) cmdKnowledge(args []string) int {
	if len(args) == 0 {
		fmt.Print(`Knowledge base — persistent learnings shared across agent sessions

Commands:
  knowledge list [agent]           List all entries, or only for <agent>
  knowledge get <agent> <key>      Retrieve a stored value
  knowledge set <agent> <key> <value>  Store or update a learning
  knowledge delete <agent> <key>   Remove a knowledge entry

Usage: ssh <host> knowledge <command> [args]
`)
		return 0
	}

	sub := args[0]
	switch sub {
	case "list":
		var (
			resp []byte
			err  error
		)
		if len(args) >= 2 {
			resp, err = h.get("/api/knowledge/" + args[1])
		} else {
			resp, err = h.get("/api/knowledge")
		}
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var entries []map[string]interface{}
		json.Unmarshal(resp, &entries)
		if len(entries) == 0 {
			fmt.Println("No knowledge entries.")
			return 0
		}
		fmt.Printf("%-20s %-30s %-40s %s\n", "AGENT", "KEY", "VALUE", "UPDATED")
		for _, e := range entries {
			agent, _ := e["agent"].(string)
			key, _ := e["key"].(string)
			value, _ := e["value"].(string)
			updated, _ := e["updated_at"].(string)
			// Truncate long values for tabular display
			display := value
			if len(display) > 38 {
				display = display[:35] + "..."
			}
			fmt.Printf("%-20s %-30s %-40s %s\n", agent, key, display, updated)
		}
		return 0

	case "get":
		if len(args) < 3 {
			fmt.Println("Usage: ssh <host> knowledge get <agent> <key>")
			return 1
		}
		agent := args[1]
		key := args[2]
		resp, err := h.get("/api/knowledge/" + agent + "/" + key)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var entry map[string]interface{}
		json.Unmarshal(resp, &entry)
		value, _ := entry["value"].(string)
		fmt.Println(value)
		return 0

	case "set":
		if len(args) < 4 {
			fmt.Println("Usage: ssh <host> knowledge set <agent> <key> <value>")
			return 1
		}
		agent := args[1]
		key := args[2]
		value := strings.Join(args[3:], " ")
		body, _ := json.Marshal(map[string]string{"value": value})
		_, err := h.put("/api/knowledge/"+agent+"/"+key, body)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		fmt.Printf("Stored: %s/%s\n", agent, key)
		return 0

	case "delete":
		if len(args) < 3 {
			fmt.Println("Usage: ssh <host> knowledge delete <agent> <key>")
			return 1
		}
		agent := args[1]
		key := args[2]
		_, err := h.delete("/api/knowledge/" + agent + "/" + key)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		fmt.Printf("Deleted: %s/%s\n", agent, key)
		return 0

	default:
		fmt.Printf("Unknown knowledge command: %s\n", sub)
		return 1
	}
}
