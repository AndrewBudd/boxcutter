package ssh

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// Handler dispatches SSH ForceCommand actions to the orchestrator HTTP API.
type Handler struct {
	apiBase string
}

func NewHandler(apiBase string) *Handler {
	return &Handler{apiBase: apiBase}
}

// Run executes the SSH command and writes output to stdout.
func (h *Handler) Run(args []string) int {
	if len(args) == 0 {
		h.printHelp()
		return 0
	}

	action := args[0]
	target := ""
	if len(args) > 1 {
		target = args[1]
	}

	switch action {
	case "new":
		return h.cmdNew(args[1:])
	case "list":
		return h.cmdList()
	case "destroy":
		if target == "" {
			fmt.Println("Usage: ssh <host> destroy <vm-name>")
			return 1
		}
		return h.cmdDestroy(target)
	case "stop":
		if target == "" {
			fmt.Println("Usage: ssh <host> stop <vm-name>")
			return 1
		}
		return h.cmdStop(target)
	case "start":
		if target == "" {
			fmt.Println("Usage: ssh <host> start <vm-name>")
			return 1
		}
		return h.cmdStart(target)
	case "cp", "copy":
		if target == "" {
			fmt.Println("Usage: ssh <host> cp <source-vm> [new-name]")
			return 1
		}
		dstName := ""
		if len(args) > 2 {
			dstName = args[2]
		}
		return h.cmdCopy(target, dstName)
	case "images":
		return h.cmdImages()
	case "golden":
		if len(args) < 3 || args[1] != "set-head" {
			fmt.Println("Usage: ssh <host> golden set-head <version>")
			return 1
		}
		return h.cmdGoldenSetHead(args[2])
	case "status":
		return h.cmdStatus()
	case "nodes":
		return h.cmdNodes()
	case "adduser":
		if target == "" {
			fmt.Println("Usage: ssh <host> adduser <github-username>")
			return 1
		}
		return h.cmdAddUser(target)
	case "removeuser":
		if target == "" {
			fmt.Println("Usage: ssh <host> removeuser <github-username>")
			return 1
		}
		return h.cmdRemoveUser(target)
	case "keys":
		return h.cmdListKeys()
	case "help":
		h.printHelp()
		return 0
	default:
		h.printHelp()
		return 1
	}
}

func (h *Handler) cmdNew(args []string) int {
	body := map[string]interface{}{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--clone":
			if i+1 < len(args) {
				body["clone_url"] = args[i+1]
				i++
			}
		case "--vcpu":
			if i+1 < len(args) {
				var n int
				fmt.Sscanf(args[i+1], "%d", &n)
				body["vcpu"] = n
				i++
			}
		case "--ram":
			if i+1 < len(args) {
				var n int
				fmt.Sscanf(args[i+1], "%d", &n)
				body["ram_mib"] = n
				i++
			}
		case "--mode":
			if i+1 < len(args) {
				body["mode"] = args[i+1]
				i++
			}
		case "--disk":
			if i+1 < len(args) {
				body["disk"] = args[i+1]
				i++
			}
		case "--node":
			if i+1 < len(args) {
				body["node_id"] = args[i+1]
				i++
			}
		}
	}

	resp, err := h.postStream("/api/vms", body, func(evt map[string]interface{}) {
		phase, _ := evt["phase"].(string)
		message, _ := evt["message"].(string)
		if phase != "ready" && phase != "error" && message != "" {
			fmt.Printf("  → %s\n", message)
		}
	})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}

	name, _ := resp["name"].(string)
	tsIP, _ := resp["tailscale_ip"].(string)
	nodeName, _ := resp["node"].(string)
	mode, _ := resp["mode"].(string)
	status, _ := resp["status"].(string)
	vcpu, _ := resp["vcpu"].(float64)
	ramMIB, _ := resp["ram_mib"].(float64)
	disk, _ := resp["disk"].(string)

	if mode == "" {
		mode = "normal"
	}
	if status == "" {
		status = "running"
	}

	fmt.Println()
	fmt.Printf("  Name:    %s\n", name)
	fmt.Printf("  Node:    %s\n", nodeName)
	if vcpu > 0 {
		fmt.Printf("  vCPU:    %.0f\n", vcpu)
	}
	if ramMIB > 0 {
		fmt.Printf("  RAM:     %.0fG\n", ramMIB/1024)
	}
	if disk != "" {
		fmt.Printf("  Disk:    %s\n", disk)
	}
	if tsIP != "" {
		fmt.Printf("  IP:      %s\n", tsIP)
		if fqdn := tailnetFQDN(name); fqdn != "" {
			fmt.Printf("  FQDN:    %s\n", fqdn)
		}
	}
	fmt.Printf("  Mode:    %s\n", mode)
	fmt.Printf("  Status:  %s\n", status)
	fmt.Println()
	if tsIP != "" {
		fmt.Printf("  Connect: ssh %s\n", name)
	} else {
		fmt.Println("  Tailscale IP pending — check with: ssh <host> list")
	}
	return 0
}

func (h *Handler) cmdList() int {
	resp, err := h.get("/api/vms")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}

	var vms []map[string]interface{}
	json.Unmarshal(resp, &vms)

	fmt.Printf("%-20s %-18s %-12s %-8s %-8s %-8s %-8s\n",
		"NAME", "TAILSCALE IP", "NODE", "MODE", "VCPU", "RAM", "STATUS")
	for _, v := range vms {
		name, _ := v["name"].(string)
		tsIP, _ := v["tailscale_ip"].(string)
		nodeName, _ := v["node_name"].(string)
		mode, _ := v["mode"].(string)
		vcpu, _ := v["vcpu"].(float64)
		ramMIB, _ := v["ram_mib"].(float64)
		status, _ := v["status"].(string)
		if tsIP == "" {
			tsIP = "-"
		}

		fmt.Printf("%-20s %-18s %-12s %-8s %-8.0f %-8s %-8s\n",
			name, tsIP, nodeName, mode, vcpu, fmt.Sprintf("%.0fG", ramMIB/1024), status)
	}
	return 0
}

func (h *Handler) cmdDestroy(name string) int {
	_, err := h.delete("/api/vms/" + name)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	fmt.Printf("VM '%s' destroyed.\n", name)
	return 0
}

func (h *Handler) cmdStop(name string) int {
	_, err := h.post("/api/vms/"+name+"/stop", nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	fmt.Printf("VM '%s' stopped.\n", name)
	return 0
}

func (h *Handler) cmdStart(name string) int {
	resp, err := h.post("/api/vms/"+name+"/start", nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var result map[string]interface{}
	json.Unmarshal(resp, &result)
	tsIP, _ := result["tailscale_ip"].(string)
	fmt.Printf("VM '%s' started.\n", name)
	if tsIP != "" {
		fmt.Printf("Connect: ssh %s\n", tsIP)
	}
	return 0
}

func (h *Handler) cmdStatus() int {
	resp, err := h.get("/api/health")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var result map[string]interface{}
	json.Unmarshal(resp, &result)

	nodesTotal, _ := result["nodes_total"].(float64)
	nodesActive, _ := result["nodes_active"].(float64)
	vmsTotal, _ := result["vms_total"].(float64)
	ramTotal, _ := result["ram_total_mib"].(float64)
	ramAlloc, _ := result["ram_allocated_mib"].(float64)

	fmt.Printf("Nodes:    %.0f active / %.0f total\n", nodesActive, nodesTotal)
	fmt.Printf("VMs:      %.0f\n", vmsTotal)
	fmt.Printf("RAM:      %.0fGB allocated / %.0fGB total\n", ramAlloc/1024, ramTotal/1024)
	fmt.Printf("Headroom: %.0fGB\n", (ramTotal-ramAlloc)/1024)
	return 0
}

func (h *Handler) cmdNodes() int {
	resp, err := h.get("/api/nodes")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}

	var nodes []map[string]interface{}
	json.Unmarshal(resp, &nodes)

	fmt.Printf("%-12s %-20s %-16s %-16s %-8s %-10s %-10s %-4s\n",
		"ID", "NAME", "BRIDGE IP", "TAILSCALE IP", "STATUS", "RAM USED", "RAM TOTAL", "VMs")
	for _, n := range nodes {
		id, _ := n["id"].(string)
		name, _ := n["tailscale_name"].(string)
		tsIP, _ := n["tailscale_ip"].(string)
		bridgeIP, _ := n["bridge_ip"].(string)
		status, _ := n["status"].(string)
		ramAlloc, _ := n["ram_allocated_mib"].(float64)
		ramTotal, _ := n["ram_total_mib"].(float64)
		vmsRunning, _ := n["vms_running"].(float64)
		if bridgeIP == "" {
			bridgeIP = "-"
		}
		if tsIP == "" {
			tsIP = "-"
		}

		// Show "-" for nodes we can't reach
		ramUsedStr := "-"
		ramTotalStr := "-"
		vmsStr := "-"
		if ramTotal > 0 {
			ramUsedStr = fmt.Sprintf("%.0fG", ramAlloc/1024)
			ramTotalStr = fmt.Sprintf("%.0fG", ramTotal/1024)
			vmsStr = fmt.Sprintf("%.0f", vmsRunning)
		}

		fmt.Printf("%-12s %-20s %-16s %-16s %-8s %-10s %-10s %-4s\n",
			id, name, bridgeIP, tsIP, status,
			ramUsedStr, ramTotalStr, vmsStr)
	}
	return 0
}

func (h *Handler) cmdCopy(srcName, dstName string) int {
	body := map[string]interface{}{}
	if dstName != "" {
		body["dst_name"] = dstName
	}

	resp, err := h.postStream("/api/vms/"+srcName+"/copy", body, func(evt map[string]interface{}) {
		phase, _ := evt["phase"].(string)
		message, _ := evt["message"].(string)
		if phase != "ready" && phase != "error" && message != "" {
			fmt.Printf("  -> %s\n", message)
		}
	})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}

	name, _ := resp["name"].(string)
	tsIP, _ := resp["tailscale_ip"].(string)
	nodeName, _ := resp["node"].(string)
	mode, _ := resp["mode"].(string)
	status, _ := resp["status"].(string)

	fmt.Println()
	fmt.Printf("  Copied:  %s -> %s\n", srcName, name)
	fmt.Printf("  Node:    %s\n", nodeName)
	if tsIP != "" {
		fmt.Printf("  IP:      %s\n", tsIP)
	}
	fmt.Printf("  Mode:    %s\n", mode)
	fmt.Printf("  Status:  %s\n", status)
	fmt.Println()
	if tsIP != "" {
		fmt.Printf("  Connect: ssh %s\n", name)
	}
	return 0
}

func (h *Handler) cmdImages() int {
	// Get golden head version
	headResp, _ := h.get("/api/golden/head")
	var headResult map[string]string
	json.Unmarshal(headResp, &headResult)
	head := headResult["version"]

	resp, err := h.get("/api/golden")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var images []map[string]interface{}
	json.Unmarshal(resp, &images)

	if head != "" {
		fmt.Printf("HEAD: %s\n\n", head)
	}

	if len(images) == 0 {
		fmt.Println("No golden images found. Images are discovered from nodes every 30 seconds.")
		return 0
	}

	fmt.Printf("%-40s %s\n", "VERSION", "NODES")
	for _, img := range images {
		version, _ := img["version"].(string)
		nodesRaw, _ := img["nodes"].([]interface{})
		var nodeNames []string
		for _, n := range nodesRaw {
			if s, ok := n.(string); ok {
				nodeNames = append(nodeNames, s)
			}
		}
		marker := ""
		if version == head {
			marker = " ← head"
		}
		fmt.Printf("%-40s %s%s\n", version, strings.Join(nodeNames, ", "), marker)
	}
	return 0
}

func (h *Handler) cmdGoldenSetHead(version string) int {
	result, err := h.post("/api/golden/head", map[string]string{"version": version})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var res map[string]interface{}
	json.Unmarshal(result, &res)
	fmt.Printf("Golden head set to %s\n", version)
	fmt.Println("Nodes will pull the new version automatically via MQTT.")
	return 0
}

func (h *Handler) cmdAddUser(githubUser string) int {
	// Fetch SSH keys from GitHub
	resp, err := http.Get(fmt.Sprintf("https://github.com/%s.keys", githubUser))
	if err != nil {
		fmt.Printf("Error fetching keys: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	keysStr := strings.TrimSpace(string(body))
	if keysStr == "" {
		fmt.Printf("No SSH keys found for GitHub user '%s'\n", githubUser)
		return 1
	}

	keys := strings.Split(keysStr, "\n")
	data := map[string]interface{}{
		"github_user": githubUser,
		"keys":        keys,
	}

	result, err := h.post("/api/keys/add", data)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var res map[string]interface{}
	json.Unmarshal(result, &res)
	added, _ := res["keys_added"].(float64)
	fmt.Printf("Added %.0f key(s) for %s. New VMs will include these keys.\n", added, githubUser)
	return 0
}

func (h *Handler) cmdRemoveUser(githubUser string) int {
	_, err := h.delete("/api/keys/" + githubUser)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	fmt.Printf("Removed keys for %s.\n", githubUser)
	return 0
}

func (h *Handler) cmdListKeys() int {
	resp, err := h.get("/api/keys")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var keys []map[string]interface{}
	json.Unmarshal(resp, &keys)

	if len(keys) == 0 {
		fmt.Println("No SSH keys configured. Use: ssh <host> adduser <github-username>")
		return 0
	}

	fmt.Printf("%-20s %-50s\n", "GITHUB USER", "KEY (truncated)")
	for _, k := range keys {
		user, _ := k["github_user"].(string)
		pubkey, _ := k["public_key"].(string)
		if len(pubkey) > 50 {
			pubkey = pubkey[:47] + "..."
		}
		fmt.Printf("%-20s %-50s\n", user, pubkey)
	}
	return 0
}

func (h *Handler) printHelp() {
	fmt.Print(`Boxcutter — ephemeral dev environments

Commands:
  new [options]           Create and start a new VM
    --clone <repo>          Clone repo on creation
    --vcpu <N>              CPU cores (default: 2)
    --ram <MiB>             RAM in MiB (default: 2048)
    --disk <size>           Disk size (default: 50G)
    --mode normal|paranoid  Network mode (default: normal)
    --node <node-id>        Pin to specific node
  list                    List all VMs
  destroy <name>          Destroy a VM
  stop <name>             Stop a running VM
  start <name>            Start a stopped VM
  cp <name> [new-name]    Copy a VM (clone its disk)
  images                  List golden images across all nodes
  golden set-head <ver>   Set golden image head version (nodes pull via MQTT)
  status                  Cluster capacity summary
  nodes                   List all nodes
  adduser <github-user>   Add SSH keys from GitHub (for new VMs)
  removeuser <github-user>
                          Remove SSH keys for a user
  keys                    List all configured SSH keys
  help                    Show this help

Usage: ssh <host> <command> [args]
`)
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

func (h *Handler) delete(path string) ([]byte, error) {
	req, _ := http.NewRequest("DELETE", h.apiBase+path, nil)
	resp, err := http.DefaultClient.Do(req)
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

// tailnetFQDN returns name.tailnet.ts.net by querying local Tailscale.
func tailnetFQDN(name string) string {
	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return ""
	}
	var status struct {
		MagicDNSSuffix string `json:"MagicDNSSuffix"`
	}
	if json.Unmarshal(out, &status) != nil || status.MagicDNSSuffix == "" {
		return ""
	}
	return name + "." + status.MagicDNSSuffix
}

// Main is called from the boxcutter-ssh-orchestrator script.
func Main() {
	apiBase := os.Getenv("BOXCUTTER_API")
	if apiBase == "" {
		apiBase = "http://localhost:8801"
	}

	command := os.Getenv("SSH_ORIGINAL_COMMAND")
	if command == "" {
		command = "help"
	}

	args := strings.Fields(command)
	handler := NewHandler(apiBase)
	os.Exit(handler.Run(args))
}
