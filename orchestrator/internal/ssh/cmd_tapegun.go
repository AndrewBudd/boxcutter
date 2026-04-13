package ssh

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (h *Handler) cmdTapegun(args []string) int {
	if len(args) == 0 {
		fmt.Print(`Tapegun — monitor and message VMs

Commands:
  tapegun activity              Show activity for all VMs
  tapegun activity <name>       Show activity for a specific VM
  tapegun wait <name>           Block until VM agent becomes idle
  tapegun send <name> <message> Send a message to a VM
  tapegun broadcast <message>   Broadcast a message to all running VMs
  tapegun sendkeys <name> <cmd> Inject a command into a VM's tmux pane

Usage: ssh <host> tapegun <command> [args]
`)
		return 0
	}

	sub := args[0]
	switch sub {
	case "activity":
		if len(args) > 1 {
			return h.tapegunActivityVM(args[1])
		}
		return h.tapegunActivityAll()

	case "wait":
		if len(args) < 2 {
			fmt.Println("Usage: ssh <host> tapegun wait <vm-name>")
			return 1
		}
		return h.tapegunWait(args[1])

	case "send":
		if len(args) < 3 {
			fmt.Println("Usage: ssh <host> tapegun send <vm-name> <message>")
			return 1
		}
		msg := strings.Join(args[2:], " ")
		return h.tapegunSend(args[1], msg, false)

	case "sendkeys":
		if len(args) < 3 {
			fmt.Println("Usage: ssh <host> tapegun sendkeys [--no-enter] <vm-name> <command>")
			return 1
		}
		// Parse --no-enter flag
		noEnter := false
		sendkeysArgs := args[1:]
		if len(sendkeysArgs) > 0 && sendkeysArgs[0] == "--no-enter" {
			noEnter = true
			sendkeysArgs = sendkeysArgs[1:]
		}
		if len(sendkeysArgs) < 2 {
			fmt.Println("Usage: ssh <host> tapegun sendkeys [--no-enter] <vm-name> <command>")
			return 1
		}
		msg := strings.Join(sendkeysArgs[1:], " ")
		return h.tapegunSend(sendkeysArgs[0], msg, true, noEnter)

	case "broadcast":
		if len(args) < 2 {
			fmt.Println("Usage: ssh <host> tapegun broadcast <message>")
			return 1
		}
		msg := strings.Join(args[1:], " ")
		return h.tapegunBroadcast(msg)

	default:
		fmt.Printf("Unknown tapegun command: %s\n", sub)
		return 1
	}
}

func (h *Handler) tapegunActivityAll() int {
	resp, err := h.get("/api/tapegun/activity")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}

	var entries []map[string]interface{}
	json.Unmarshal(resp, &entries)

	if len(entries) == 0 {
		fmt.Println("No VMs found.")
		return 0
	}

	for i, e := range entries {
		name, _ := e["name"].(string)
		nodeName, _ := e["node_name"].(string)
		vmStatus, _ := e["vm_status"].(string)
		pending, _ := e["pending_messages"].(float64)

		fmt.Printf("=== %s (node: %s, status: %s, pending: %.0f) ===\n",
			name, nodeName, vmStatus, pending)

		// Show agent status from Claude Code hooks (Stop/UserPromptSubmit)
		if agentStatus, ok := e["agent_status"].(map[string]interface{}); ok {
			aStatus, _ := agentStatus["status"].(string)
			aTs, _ := agentStatus["timestamp"].(string)
			fmt.Printf("Agent:    %s (since: %s)\n", aStatus, aTs)
		}

		if activity, ok := e["activity"].(map[string]interface{}); ok {
			status, _ := activity["status"].(string)
			pane, _ := activity["pane_content"].(string)
			ts, _ := activity["timestamp"].(string)
			fmt.Printf("Activity: %s (last report: %s)\n", status, ts)
			if pane != "" {
				// Show last 10 lines of pane content
				lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
				start := 0
				if len(lines) > 10 {
					start = len(lines) - 10
				}
				for _, line := range lines[start:] {
					fmt.Printf("  %s\n", line)
				}
			}
		} else {
			fmt.Println("Activity: no data (daemon may not be running)")
		}
		if i < len(entries)-1 {
			fmt.Println()
		}
	}
	return 0
}

func (h *Handler) tapegunActivityVM(name string) int {
	resp, err := h.get("/api/tapegun/activity/" + name)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}

	var e map[string]interface{}
	json.Unmarshal(resp, &e)

	vmStatus, _ := e["vm_status"].(string)
	nodeName, _ := e["node_name"].(string)
	pending, _ := e["pending_messages"].(float64)

	fmt.Printf("VM:       %s\n", name)
	fmt.Printf("Node:     %s\n", nodeName)
	fmt.Printf("Status:   %s\n", vmStatus)
	fmt.Printf("Pending:  %.0f message(s)\n", pending)

	// Show agent status from Claude Code hooks
	if agentStatus, ok := e["agent_status"].(map[string]interface{}); ok {
		aStatus, _ := agentStatus["status"].(string)
		aTs, _ := agentStatus["timestamp"].(string)
		fmt.Printf("Agent:    %s (since: %s)\n", aStatus, aTs)
	}

	if activity, ok := e["activity"].(map[string]interface{}); ok {
		status, _ := activity["status"].(string)
		pane, _ := activity["pane_content"].(string)
		ts, _ := activity["timestamp"].(string)
		fmt.Printf("Activity: %s (last: %s)\n", status, ts)
		if pane != "" {
			fmt.Println("--- pane content ---")
			fmt.Print(pane)
			if !strings.HasSuffix(pane, "\n") {
				fmt.Println()
			}
			fmt.Println("--- end ---")
		}
	} else {
		fmt.Println("Activity: no data")
	}
	return 0
}

func (h *Handler) tapegunWait(name string) int {
	fmt.Printf("Waiting for %s to become idle...\n", name)
	for {
		resp, err := h.get("/api/tapegun/activity/" + name)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}

		var e map[string]interface{}
		json.Unmarshal(resp, &e)

		// Check agent status from Claude Code hooks (most reliable)
		if agentStatus, ok := e["agent_status"].(map[string]interface{}); ok {
			aStatus, _ := agentStatus["status"].(string)
			if aStatus == "idle" || aStatus == "stopped" || aStatus == "error" {
				fmt.Printf("%s is %s\n", name, aStatus)
				return 0
			}
		}

		// Fallback: check daemon activity status
		if activity, ok := e["activity"].(map[string]interface{}); ok {
			status, _ := activity["status"].(string)
			if status == "idle" || status == "stopped" {
				fmt.Printf("%s is %s\n", name, status)
				return 0
			}
		}

		time.Sleep(5 * time.Second)
	}
}

func (h *Handler) tapegunSend(name, body string, sendKeys bool, noEnter ...bool) int {
	msg := map[string]interface{}{
		"body":      body,
		"from":      "ssh-user",
		"priority":  "normal",
		"send_keys": sendKeys,
	}
	if len(noEnter) > 0 && noEnter[0] {
		msg["no_enter"] = true
	}

	resp, err := h.post("/api/tapegun/message/"+name, msg)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var result map[string]string
	json.Unmarshal(resp, &result)
	fmt.Printf("Message sent to %s (id: %s)\n", name, result["message_id"])
	return 0
}

func (h *Handler) tapegunBroadcast(body string) int {
	msg := map[string]interface{}{
		"body":     body,
		"from":     "ssh-user",
		"priority": "normal",
		"filter":   "running",
	}

	resp, err := h.post("/api/tapegun/broadcast", msg)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var result map[string]interface{}
	json.Unmarshal(resp, &result)

	if sent, ok := result["sent"].([]interface{}); ok && len(sent) > 0 {
		names := make([]string, len(sent))
		for i, s := range sent {
			names[i], _ = s.(string)
		}
		fmt.Printf("Broadcast sent to: %s\n", strings.Join(names, ", "))
	}
	if failed, ok := result["failed"].([]interface{}); ok && len(failed) > 0 {
		names := make([]string, len(failed))
		for i, s := range failed {
			names[i], _ = s.(string)
		}
		fmt.Printf("Failed: %s\n", strings.Join(names, ", "))
	}
	return 0
}

