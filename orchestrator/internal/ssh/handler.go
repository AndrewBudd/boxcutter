package ssh

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/team"
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
	case "logs":
		if target == "" {
			fmt.Println("Usage: ssh <host> logs <vm-name> [--lines N]")
			return 1
		}
		return h.cmdLogs(target, args[2:])
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
	case "describe":
		if target == "" || len(args) < 3 {
			fmt.Println("Usage: ssh <host> describe <vm-name> <description>")
			return 1
		}
		desc := strings.Join(args[2:], " ")
		return h.cmdDescribe(target, desc)
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
	case "repos":
		return h.cmdRepos(args[1:])
	case "projects":
		return h.cmdProjects(args[1:])
	case "tapegun":
		return h.cmdTapegun(args[1:])
	case "msg":
		return h.cmdMsg(args[1:])
	case "authorize":
		if target == "" {
			fmt.Println("Usage: ssh <host> authorize <vm-name>")
			return 1
		}
		return h.cmdAuthorize(target)
	case "exec":
		if target == "" || len(args) < 3 {
			fmt.Println("Usage: ssh <host> exec <vm-name> <command...>")
			return 1
		}
		cmd := strings.Join(args[2:], " ")
		return h.cmdExec(target, cmd)
	case "cp-to":
		if len(args) < 4 {
			fmt.Println("Usage: ssh <host> cp-to <vm-name> <local-path|->" + " <remote-path>")
			return 1
		}
		return h.cmdCopyTo(args[1], args[2], args[3])
	case "cp-from":
		if len(args) < 4 {
			fmt.Println("Usage: ssh <host> cp-from <vm-name> <remote-path> <local-path|->")
			return 1
		}
		return h.cmdCopyFrom(args[1], args[2], args[3])
	case "team":
		return h.cmdTeam(args[1:])
	case "queue":
		return h.cmdQueue(args[1:])
	case "knowledge":
		return h.cmdKnowledge(args[1:])
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
	var cloneURLs []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--clone":
			if i+1 < len(args) {
				cloneURLs = append(cloneURLs, args[i+1])
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
		case "--type":
			if i+1 < len(args) {
				body["type"] = args[i+1]
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
		case "--name":
			if i+1 < len(args) {
				body["name"] = args[i+1]
				i++
			}
		case "--desc", "--description":
			if i+1 < len(args) {
				body["description"] = args[i+1]
				i++
			}
		case "--ts-authkey":
			if i+1 < len(args) {
				body["tailscale_authkey"] = args[i+1]
				i++
			}
		case "--agent-config":
			if i+1 < len(args) {
				var raw json.RawMessage
				if err := json.Unmarshal([]byte(args[i+1]), &raw); err == nil {
					body["agent_config"] = raw
				}
				i++
			}
		}
	}

	// Set clone URLs in request body
	if len(cloneURLs) == 1 {
		body["clone_url"] = cloneURLs[0]
	} else if len(cloneURLs) > 1 {
		body["clone_urls"] = cloneURLs
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
		fmt.Printf("  RAM:     %s\n", formatRAM(ramMIB))
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

	fmt.Printf("%-20s %-18s %-12s %-8s %-5s %-8s %-8s %-8s\n",
		"NAME", "TAILSCALE IP", "NODE", "TYPE", "VCPU", "RAM", "MODE", "STATUS")
	for _, v := range vms {
		name, _ := v["name"].(string)
		tsIP, _ := v["tailscale_ip"].(string)
		nodeName, _ := v["node_name"].(string)
		vmType, _ := v["type"].(string)
		desc, _ := v["description"].(string)
		mode, _ := v["mode"].(string)
		vcpu, _ := v["vcpu"].(float64)
		ramMIB, _ := v["ram_mib"].(float64)
		status, _ := v["status"].(string)
		if tsIP == "" {
			tsIP = "-"
		}
		if vmType == "" || vmType == "firecracker" {
			vmType = "fc"
		}

		fmt.Printf("%-20s %-18s %-12s %-8s %-5.0f %-8s %-8s %-8s\n",
			name, tsIP, nodeName, vmType, vcpu, formatRAM(ramMIB), mode, status)
		if desc != "" {
			fmt.Printf("  %s\n", desc)
		}
	}
	return 0
}

func (h *Handler) cmdDescribe(name, description string) int {
	body := map[string]interface{}{"description": description}
	data, _ := json.Marshal(body)
	_, err := h.patch("/api/vms/"+name, data)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	fmt.Printf("VM '%s' description updated.\n", name)
	return 0
}

func (h *Handler) cmdLogs(name string, args []string) int {
	lines := "100"
	for i := 0; i < len(args); i++ {
		if args[i] == "--lines" && i+1 < len(args) {
			lines = args[i+1]
			i++
		}
	}

	resp, err := h.get(fmt.Sprintf("/api/vms/%s/logs?lines=%s", name, lines))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	fmt.Print(string(resp))
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
	fmt.Printf("RAM:      %s allocated / %s total\n", formatRAM(ramAlloc), formatRAM(ramTotal))
	fmt.Printf("Headroom: %s\n", formatRAM(ramTotal-ramAlloc))
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
			ramUsedStr = formatRAM(ramAlloc)
			ramTotalStr = formatRAM(ramTotal)
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

func (h *Handler) cmdRepos(args []string) int {
	if len(args) < 1 {
		fmt.Println("Usage: ssh <host> repos <list|add|remove> <vm-name> [repo]")
		return 1
	}

	action := args[0]
	switch action {
	case "list":
		if len(args) < 2 {
			fmt.Println("Usage: ssh <host> repos list <vm-name>")
			return 1
		}
		vmName := args[1]
		resp, err := h.get("/api/vms/" + vmName + "/repos")
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var result struct {
			Repos []string `json:"repos"`
		}
		json.Unmarshal(resp, &result)
		if len(result.Repos) == 0 {
			fmt.Printf("No repos configured for %s.\n", vmName)
			return 0
		}
		fmt.Printf("Repos for %s:\n", vmName)
		for _, r := range result.Repos {
			fmt.Printf("  %s\n", r)
		}
		return 0

	case "add":
		if len(args) < 3 {
			fmt.Println("Usage: ssh <host> repos add <vm-name> <owner/repo>")
			return 1
		}
		vmName := args[1]
		repo := args[2]
		result, err := h.post("/api/vms/"+vmName+"/repos", map[string]string{"repo": repo})
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var res struct {
			Repos []string `json:"repos"`
		}
		json.Unmarshal(result, &res)
		fmt.Printf("Added %s. Repos for %s:\n", repo, vmName)
		for _, r := range res.Repos {
			fmt.Printf("  %s\n", r)
		}
		return 0

	case "remove":
		if len(args) < 3 {
			fmt.Println("Usage: ssh <host> repos remove <vm-name> <owner/repo>")
			return 1
		}
		vmName := args[1]
		repo := args[2]
		result, err := h.delete("/api/vms/" + vmName + "/repos/" + repo)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var res struct {
			Repos []string `json:"repos"`
		}
		json.Unmarshal(result, &res)
		fmt.Printf("Removed %s. Repos for %s:\n", repo, vmName)
		for _, r := range res.Repos {
			fmt.Printf("  %s\n", r)
		}
		if len(res.Repos) == 0 {
			fmt.Println("  (none)")
		}
		return 0

	default:
		fmt.Println("Usage: ssh <host> repos <list|add|remove> <vm-name> [repo]")
		return 1
	}
}

func (h *Handler) cmdProjects(args []string) int {
	if len(args) < 1 {
		fmt.Println("Usage: ssh <host> projects <list|add|remove> <vm-name> [project]")
		return 1
	}
	switch args[0] {
	case "list":
		if len(args) < 2 {
			fmt.Println("Usage: ssh <host> projects list <vm-name>")
			return 1
		}
		resp, err := h.get("/api/vms/" + args[1] + "/projects")
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var result struct{ Projects []string `json:"projects"` }
		json.Unmarshal(resp, &result)
		if len(result.Projects) == 0 {
			fmt.Printf("No projects configured for %s.\n", args[1])
			return 0
		}
		fmt.Printf("Projects for %s:\n", args[1])
		for _, p := range result.Projects {
			fmt.Printf("  %s\n", p)
		}
		return 0
	case "add":
		if len(args) < 3 {
			fmt.Println("Usage: ssh <host> projects add <vm-name> <owner/number>")
			return 1
		}
		result, err := h.post("/api/vms/"+args[1]+"/projects", map[string]string{"project": args[2]})
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var res struct{ Projects []string `json:"projects"` }
		json.Unmarshal(result, &res)
		fmt.Printf("Added %s. Projects for %s:\n", args[2], args[1])
		for _, p := range res.Projects {
			fmt.Printf("  %s\n", p)
		}
		return 0
	case "remove":
		if len(args) < 3 {
			fmt.Println("Usage: ssh <host> projects remove <vm-name> <owner/number>")
			return 1
		}
		result, err := h.delete("/api/vms/" + args[1] + "/projects/" + args[2])
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		var res struct{ Projects []string `json:"projects"` }
		json.Unmarshal(result, &res)
		fmt.Printf("Removed %s. Projects for %s:\n", args[2], args[1])
		for _, p := range res.Projects {
			fmt.Printf("  %s\n", p)
		}
		if len(res.Projects) == 0 {
			fmt.Println("  (none)")
		}
		return 0
	default:
		fmt.Println("Usage: ssh <host> projects <list|add|remove> <vm-name> [project]")
		return 1
	}
}

func (h *Handler) cmdExec(name, command string) int {
	resp, err := h.post("/api/vms/"+name+"/exec", map[string]string{"command": command})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	var result struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exit_code"`
	}
	json.Unmarshal(resp, &result)
	fmt.Print(result.Output)
	return result.ExitCode
}

func (h *Handler) cmdCopyTo(vmName, srcPath, dstPath string) int {
	var src io.Reader
	if srcPath == "-" {
		src = os.Stdin
	} else {
		f, err := os.Open(srcPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		defer f.Close()
		src = f
	}

	url := fmt.Sprintf("%s/api/vms/%s/cp-to?path=%s", h.apiBase, vmName, dstPath)
	resp, err := http.Post(url, "application/octet-stream", src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		return 1
	}
	return 0
}

func (h *Handler) cmdCopyFrom(vmName, srcPath, dstPath string) int {
	url := fmt.Sprintf("%s/api/vms/%s/cp-from?path=%s", h.apiBase, vmName, srcPath)
	resp, err := http.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		return 1
	}

	var dst io.Writer
	if dstPath == "-" {
		dst = os.Stdout
	} else {
		f, err := os.Create(dstPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		defer f.Close()
		dst = f
	}

	if _, err := io.Copy(dst, resp.Body); err != nil {
		fmt.Fprintf(os.Stderr, "Error copying data: %v\n", err)
		return 1
	}
	return 0
}

func (h *Handler) cmdAuthorize(name string) int {
	// Tell the VM to fetch its SSH key from the metadata service and configure SSH.
	setupScript := `set -e
METADATA="http://169.254.169.254"
mkdir -p /home/dev/.ssh
curl -sf "$METADATA/metadata/boxcutter-ssh-key" > /home/dev/.ssh/boxcutter-vm.key
chmod 600 /home/dev/.ssh/boxcutter-vm.key
printf '%s\n' \
  'Host orchestrator' \
  '  HostName 192.168.50.2' \
  '  User boxcutter' \
  '  IdentityFile ~/.ssh/boxcutter-vm.key' \
  '  StrictHostKeyChecking no' \
  '  UserKnownHostsFile /dev/null' \
  '  LogLevel ERROR' \
  > /home/dev/.ssh/config
chmod 700 /home/dev/.ssh
chmod 600 /home/dev/.ssh/config
echo "ok"`

	resp, err := h.post("/api/vms/"+name+"/exec", map[string]string{"command": setupScript})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var result map[string]string
	json.Unmarshal(resp, &result)
	if strings.TrimSpace(result["output"]) == "ok" {
		fmt.Printf("VM '%s' authorized. Inside the VM, run: boxcutter list\n", name)
	} else {
		fmt.Printf("Output: %s\n", result["output"])
	}
	return 0
}

// --- Team commands ---

func (h *Handler) cmdTeam(args []string) int {
	if len(args) == 0 {
		fmt.Println(`Usage: ssh <host> team <subcommand>

Subcommands:
  apply -f -          Create/update team from YAML (reads stdin)
  diff -f -           Show what would change (dry-run)
  export <name>       Export running team as YAML
  destroy <name>      Destroy all VMs for a team
  list                List active teams
  metrics <name>      Show token usage and cost per agent
  status <name>       Show status of team VMs`)
		return 1
	}

	switch args[0] {
	case "apply":
		return h.teamApply(args[1:])
	case "diff":
		return h.teamDiff(args[1:])
	case "export":
		return h.teamExport(args[1:])
	case "destroy":
		return h.teamDestroy(args[1:])
	case "list":
		return h.teamList(args[1:])
	case "metrics":
		return h.teamMetrics(args[1:])
	case "status":
		return h.teamStatus(args[1:])
	case "message", "msg":
		return h.teamMsg(args[1:])
	case "files":
		return h.teamFilesCmd(args[1:])
	case "conflicts":
		return h.teamConflictsCmd(args[1:])
	case "scratchpad", "scratch":
		return h.teamScratchpadCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown team subcommand: %s\n", args[0])
		return 1
	}
}

func (h *Handler) teamApply(args []string) int {
	// Parse -f flag. Only -f - (stdin) is supported.
	file := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "-f" && i+1 < len(args) {
			file = args[i+1]
			i++
		}
	}
	if file == "" {
		fmt.Fprintf(os.Stderr, "Usage: ssh <host> team apply -f -\n")
		fmt.Fprintf(os.Stderr, "  Reads team YAML from stdin. Pipe with: cat team.yaml | ssh <host> team apply -f -\n")
		return 1
	}

	var data []byte
	var err error
	if file == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		fmt.Fprintf(os.Stderr, "Error: only -f - (stdin) is supported. File paths are not available on the orchestrator.\n")
		fmt.Fprintf(os.Stderr, "Usage: cat team.yaml | ssh <host> team apply -f -\n")
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading stdin: %v\n", err)
		return 1
	}

	ts, err := team.Parse(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	agents := ts.Resolve()
	fmt.Printf("Team: %s\n", ts.Metadata.Name)
	fmt.Printf("Agents: %d definitions → %d VMs\n\n", len(ts.Spec.Agents), len(agents))

	// Collect all persona file paths across the team (for persona cleanup)
	var allPersonaFiles []string
	for _, a := range agents {
		if a.Persona != nil && a.Persona.ClaudeMD != "" {
			allPersonaFiles = append(allPersonaFiles, a.Persona.ClaudeMD)
		}
	}

	// Fetch existing VMs with details for config comparison
	existingVMs := make(map[string]map[string]interface{})
	resp, err := h.get("/api/vms")
	if err == nil {
		var vms []map[string]interface{}
		json.Unmarshal(resp, &vms)
		for _, vm := range vms {
			if name, ok := vm["name"].(string); ok {
				existingVMs[name] = vm
			}
		}
	}

	// Build agent lookup for config comparison
	agentByName := make(map[string]*team.ResolvedAgent)
	for i := range agents {
		agentByName[agents[i].VMName] = &agents[i]
	}

	// Compute diff: create, update (config change), ok (unchanged), destroy
	var toCreate []team.ResolvedAgent
	var toUpdate []team.ResolvedAgent
	var unchanged []string
	for _, a := range agents {
		live, exists := existingVMs[a.VMName]
		if !exists {
			toCreate = append(toCreate, a)
			continue
		}

		// Check for hardware changes (warn only — too destructive to auto-apply)
		var hwDiffs []string
		liveVCPU, _ := live["vcpu"].(float64)
		if int(liveVCPU) != a.VCPU && liveVCPU > 0 {
			hwDiffs = append(hwDiffs, fmt.Sprintf("vcpu: %d→%d", int(liveVCPU), a.VCPU))
		}
		liveRAM, _ := live["ram_mib"].(float64)
		if int(liveRAM) != a.RAMMiB() && liveRAM > 0 {
			hwDiffs = append(hwDiffs, fmt.Sprintf("ram: %dM→%dM", int(liveRAM), a.RAMMiB()))
		}

		// Config changes (persona, repos, tapegun) can be pushed live
		agentCfg := a.AgentConfig(ts.Metadata.Name, allPersonaFiles)
		cfgJSON, _ := json.Marshal(agentCfg)
		cfgChanged := true // assume changed; compare with live config if available

		// Try to fetch current agent-config from the VM's node to compare
		if nodeID, ok := live["node_id"].(string); ok && nodeID != "" {
			nodeResp, nodeErr := h.get("/api/nodes/" + nodeID)
			if nodeErr == nil {
				var nodeInfo map[string]interface{}
				json.Unmarshal(nodeResp, &nodeInfo)
			}
		}

		// Simple heuristic: if persona or repos are specified, always push
		// (comparing full config across the network is expensive; pushing
		// an identical config is cheap and idempotent)
		hasConfig := a.Persona != nil || len(a.Repos) > 0 || len(a.Tapegun) > 0
		if hasConfig {
			_ = cfgJSON // will be used in the update loop
			toUpdate = append(toUpdate, a)
		} else {
			cfgChanged = false
		}

		if len(hwDiffs) > 0 {
			fmt.Printf("  warning  %s  hardware change requires recreate: %s\n", a.VMName, strings.Join(hwDiffs, ", "))
		}
		if !cfgChanged && len(hwDiffs) == 0 {
			unchanged = append(unchanged, a.VMName)
		}
	}

	// Find VMs to destroy (scale-down): existing team VMs not in the spec
	wantVMs := make(map[string]bool)
	for _, a := range agents {
		wantVMs[a.VMName] = true
	}
	var toDestroy []string
	if resp != nil {
		var vms []map[string]interface{}
		json.Unmarshal(resp, &vms)
		for _, vm := range vms {
			name, _ := vm["name"].(string)
			if strings.HasPrefix(name, ts.Metadata.Name+"-") && !wantVMs[name] {
				toDestroy = append(toDestroy, name)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(toDestroy)))

	// Report unchanged
	for _, name := range unchanged {
		fmt.Printf("  ok       %s\n", name)
	}

	// Push config updates to existing VMs
	updated := 0
	for _, a := range toUpdate {
		agentCfg := a.AgentConfig(ts.Metadata.Name, allPersonaFiles)
		cfgJSON, _ := json.Marshal(agentCfg)

		live := existingVMs[a.VMName]
		nodeID, _ := live["node_id"].(string)
		if nodeID == "" {
			fmt.Printf("  update   %s ... SKIPPED (no node)\n", a.VMName)
			continue
		}

		fmt.Printf("  update   %s ...", a.VMName)
		// Push agent-config via orchestrator API → node agent → vmid
		updateBody := map[string]interface{}{
			"node_id":      nodeID,
			"agent_config": json.RawMessage(cfgJSON),
		}
		updateJSON, _ := json.Marshal(updateBody)
		_, err := h.put("/api/vms/"+a.VMName+"/agent-config", updateJSON)
		if err != nil {
			fmt.Printf(" FAILED: %v\n", err)
			continue
		}
		fmt.Printf(" ok\n")
		updated++
	}

	// Create new VMs, waiting for node capacity when necessary.
	const (
		capacityPollInterval = 10 * time.Second
		capacityTimeout      = 5 * time.Minute
	)
	created := 0
	for _, a := range toCreate {
		agentCfg := a.AgentConfig(ts.Metadata.Name, allPersonaFiles)
		agentCfgJSON, _ := json.Marshal(agentCfg)

		body := map[string]interface{}{
			"name":         a.VMName,
			"type":         a.Type,
			"vcpu":         a.VCPU,
			"ram_mib":      a.RAMMiB(),
			"disk":         a.Disk,
			"mode":         a.Mode,
			"labels":       a.Labels(ts.Metadata.Name),
			"agent_config": json.RawMessage(agentCfgJSON),
		}
		if a.Description != "" {
			body["description"] = a.Description
		}
		urls := a.CloneURLs()
		if len(urls) == 1 {
			body["clone_url"] = urls[0]
		} else if len(urls) > 1 {
			body["clone_urls"] = urls
		}

		fmt.Printf("  create   %s ...", a.VMName)
		deadline := time.Now().Add(capacityTimeout)
		var lastErr error
		for {
			_, err := h.postStream("/api/vms", body, func(evt map[string]interface{}) {
				// suppress progress for batch creation
			})
			if err == nil {
				fmt.Printf(" ok\n")
				created++
				// Auto-create messaging inbox queue for this agent (#190)
				queueName := a.VMName + ".inbox"
				h.post("/api/queues", map[string]string{"name": queueName})
				break
			}
			lastErr = err
			if !isCapacityError(err) || time.Now().After(deadline) {
				fmt.Printf(" FAILED: %v\n", lastErr)
				break
			}
			fmt.Printf(" waiting for capacity (retry in 10s)\n")
			time.Sleep(capacityPollInterval)
			fmt.Printf("  create   %s ...", a.VMName)
		}
	}

	// Destroy scale-down VMs
	destroyed := 0
	for _, name := range toDestroy {
		fmt.Printf("  destroy  %s ...", name)
		_, err := h.delete("/api/vms/" + name)
		if err != nil {
			fmt.Printf(" FAILED: %v\n", err)
			continue
		}
		fmt.Printf(" ok\n")
		destroyed++
	}

	// Auto-create team-level messaging topics (#190)
	if created > 0 {
		teamName := ts.Metadata.Name
		h.post("/api/topics", map[string]string{"name": teamName + ".broadcast"})
		h.post("/api/topics", map[string]string{"name": teamName + ".status"})
	}

	fmt.Printf("\nResult: %d created, %d updated, %d unchanged, %d destroyed\n", created, updated, len(unchanged), destroyed)
	return 0
}

// isCapacityError returns true if the error indicates no node has capacity,
// meaning a retry after auto-scaler adds nodes may succeed.
func isCapacityError(err error) bool {
	msg := err.Error()
	return msg == "all nodes failed" ||
		msg == "no active nodes" ||
		msg == "no reachable nodes"
}

func (h *Handler) teamDiff(args []string) int {
	// Parse -f flag — same stdin-only pattern as apply
	file := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "-f" && i+1 < len(args) {
			file = args[i+1]
			i++
		}
	}
	if file == "" {
		fmt.Fprintf(os.Stderr, "Usage: ssh <host> team diff -f -\n")
		fmt.Fprintf(os.Stderr, "  Reads team YAML from stdin. Pipe with: cat team.yaml | ssh <host> team diff -f -\n")
		return 1
	}

	var data []byte
	var err error
	if file == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		fmt.Fprintf(os.Stderr, "Error: only -f - (stdin) is supported.\n")
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading stdin: %v\n", err)
		return 1
	}

	ts, err := team.Parse(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	agents := ts.Resolve()

	// Fetch live VMs
	resp, err := h.get("/api/vms")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing VMs: %v\n", err)
		return 1
	}
	var liveVMs []map[string]interface{}
	json.Unmarshal(resp, &liveVMs)

	liveByName := make(map[string]map[string]interface{})
	for _, vm := range liveVMs {
		if name, ok := vm["name"].(string); ok {
			liveByName[name] = vm
		}
	}

	// Build wanted set
	wantVMs := make(map[string]bool)
	for _, a := range agents {
		wantVMs[a.VMName] = true
	}

	// Compute diff for each desired agent
	nOK, nCreate, nUpdate, nDestroy := 0, 0, 0, 0

	fmt.Printf("Team: %s\n", ts.Metadata.Name)
	fmt.Printf("Agents: %d definitions → %d VMs\n\n", len(ts.Spec.Agents), len(agents))

	for _, a := range agents {
		live, exists := liveByName[a.VMName]
		if !exists {
			fmt.Printf("  + create   %-35s  %s vcpu=%d ram=%s disk=%s mode=%s\n",
				a.VMName, a.Type, a.VCPU, a.RAM, a.Disk, a.Mode)
			nCreate++
			continue
		}

		// Compare config fields
		var diffs []string

		liveVCPU, _ := live["vcpu"].(float64)
		if int(liveVCPU) != a.VCPU && liveVCPU > 0 {
			diffs = append(diffs, fmt.Sprintf("vcpu: %d → %d", int(liveVCPU), a.VCPU))
		}

		liveRAM, _ := live["ram_mib"].(float64)
		wantRAM := a.RAMMiB()
		if int(liveRAM) != wantRAM && liveRAM > 0 {
			diffs = append(diffs, fmt.Sprintf("ram: %dM → %dM", int(liveRAM), wantRAM))
		}

		liveDisk, _ := live["disk"].(string)
		if liveDisk != "" && liveDisk != a.Disk {
			diffs = append(diffs, fmt.Sprintf("disk: %s → %s", liveDisk, a.Disk))
		}

		liveType, _ := live["type"].(string)
		if liveType == "" {
			liveType = "firecracker"
		}
		if liveType != a.Type {
			diffs = append(diffs, fmt.Sprintf("type: %s → %s", liveType, a.Type))
		}

		liveMode, _ := live["mode"].(string)
		if liveMode == "" {
			liveMode = "normal"
		}
		if liveMode != a.Mode {
			diffs = append(diffs, fmt.Sprintf("mode: %s → %s", liveMode, a.Mode))
		}

		if len(diffs) > 0 {
			fmt.Printf("  ~ update   %-35s  %s\n", a.VMName, strings.Join(diffs, ", "))
			nUpdate++
		} else {
			fmt.Printf("    ok       %s\n", a.VMName)
			nOK++
		}
	}

	// Find team VMs to destroy (scale-down)
	prefix := ts.Metadata.Name + "-"
	var toDestroy []string
	for _, vm := range liveVMs {
		name, _ := vm["name"].(string)
		if strings.HasPrefix(name, prefix) && !wantVMs[name] {
			toDestroy = append(toDestroy, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(toDestroy)))

	for _, name := range toDestroy {
		fmt.Printf("  - destroy  %s\n", name)
		nDestroy++
	}

	fmt.Printf("\nSummary: %d ok, %d to create, %d to update, %d to destroy\n", nOK, nCreate, nUpdate, nDestroy)
	if nCreate == 0 && nUpdate == 0 && nDestroy == 0 {
		fmt.Println("No changes needed.")
	}
	return 0
}

func (h *Handler) teamExport(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: ssh <host> team export <team-name>\n")
		return 1
	}
	teamName := args[0]

	resp, err := h.get("/api/vms")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing VMs: %v\n", err)
		return 1
	}

	var vms []map[string]interface{}
	json.Unmarshal(resp, &vms)

	// Collect team VMs by name prefix
	prefix := teamName + "-"
	type vmInfo struct {
		name      string
		agent     string
		replica   int
		vmType    string
		vcpu      int
		ramMIB    int
		disk      string
		mode      string
		desc      string
		role      string
	}

	var teamVMs []vmInfo
	for _, vm := range vms {
		name, _ := vm["name"].(string)
		if !strings.HasPrefix(name, prefix) {
			continue
		}

		// Parse: {team}-{agent}-{N}
		suffix := name[len(prefix):]
		parts := strings.Split(suffix, "-")
		if len(parts) < 2 {
			continue
		}
		replicaStr := parts[len(parts)-1]
		replica := 0
		for _, c := range replicaStr {
			if c >= '0' && c <= '9' {
				replica = replica*10 + int(c-'0')
			}
		}
		agentName := strings.Join(parts[:len(parts)-1], "-")

		vmType, _ := vm["type"].(string)
		vcpu, _ := vm["vcpu"].(float64)
		ramMIB, _ := vm["ram_mib"].(float64)
		disk, _ := vm["disk"].(string)
		mode, _ := vm["mode"].(string)
		desc, _ := vm["description"].(string)

		// Check labels for role
		role := ""
		if labels, ok := vm["labels"].(map[string]interface{}); ok {
			if r, ok := labels["role"].(string); ok {
				role = r
			}
		}

		if vmType == "" {
			vmType = "firecracker"
		}
		if mode == "" {
			mode = "normal"
		}
		if disk == "" {
			disk = "50G"
		}

		teamVMs = append(teamVMs, vmInfo{
			name:    name,
			agent:   agentName,
			replica: replica,
			vmType:  vmType,
			vcpu:    int(vcpu),
			ramMIB:  int(ramMIB),
			disk:    disk,
			mode:    mode,
			desc:    desc,
			role:    role,
		})
	}

	if len(teamVMs) == 0 {
		fmt.Fprintf(os.Stderr, "No VMs found for team %q\n", teamName)
		return 1
	}

	// Group by agent name → count replicas and extract config
	type agentInfo struct {
		name     string
		replicas int
		vmType   string
		vcpu     int
		ramMIB   int
		disk     string
		mode     string
		desc     string
		role     string
	}
	agentMap := make(map[string]*agentInfo)
	var agentOrder []string
	for _, vm := range teamVMs {
		ai, exists := agentMap[vm.agent]
		if !exists {
			ai = &agentInfo{
				name:   vm.agent,
				vmType: vm.vmType,
				vcpu:   vm.vcpu,
				ramMIB: vm.ramMIB,
				disk:   vm.disk,
				mode:   vm.mode,
				desc:   vm.desc,
				role:   vm.role,
			}
			agentMap[vm.agent] = ai
			agentOrder = append(agentOrder, vm.agent)
		}
		ai.replicas++
	}

	// Compute defaults: most common values across agents
	typeCounts := make(map[string]int)
	vcpuCounts := make(map[int]int)
	ramCounts := make(map[int]int)
	diskCounts := make(map[string]int)
	modeCounts := make(map[string]int)
	for _, ai := range agentMap {
		typeCounts[ai.vmType]++
		vcpuCounts[ai.vcpu]++
		ramCounts[ai.ramMIB]++
		diskCounts[ai.disk]++
		modeCounts[ai.mode]++
	}
	defType := maxKey(typeCounts)
	defVCPU := maxKeyInt(vcpuCounts)
	defRAM := maxKeyInt(ramCounts)
	defDisk := maxKey(diskCounts)
	defMode := maxKey(modeCounts)

	// Emit YAML
	fmt.Printf("apiVersion: boxcutter/v1\nkind: Team\nmetadata:\n  name: %s\nspec:\n", teamName)

	// Defaults
	fmt.Printf("  defaults:\n")
	fmt.Printf("    type: %s\n", defType)
	fmt.Printf("    vcpu: %d\n", defVCPU)
	fmt.Printf("    ram: %s\n", formatRAMMiB(defRAM))
	fmt.Printf("    disk: %s\n", defDisk)
	fmt.Printf("    mode: %s\n", defMode)

	// Agents
	fmt.Printf("\n  agents:\n")
	for _, name := range agentOrder {
		ai := agentMap[name]
		fmt.Printf("    - name: %s\n", ai.name)
		if ai.replicas > 1 {
			fmt.Printf("      replicas: %d\n", ai.replicas)
		}
		// Only emit fields that differ from defaults
		if ai.vmType != defType {
			fmt.Printf("      type: %s\n", ai.vmType)
		}
		if ai.vcpu != defVCPU {
			fmt.Printf("      vcpu: %d\n", ai.vcpu)
		}
		if ai.ramMIB != defRAM {
			fmt.Printf("      ram: %s\n", formatRAMMiB(ai.ramMIB))
		}
		if ai.disk != defDisk {
			fmt.Printf("      disk: %s\n", ai.disk)
		}
		if ai.mode != defMode {
			fmt.Printf("      mode: %s\n", ai.mode)
		}
		if ai.desc != "" {
			fmt.Printf("      description: %q\n", ai.desc)
		}
		if ai.role != "" {
			fmt.Printf("      persona:\n        role: %s\n", ai.role)
		}
	}

	return 0
}

// maxKey returns the key with the highest count from a string→int map.
func maxKey(m map[string]int) string {
	best := ""
	bestN := 0
	for k, n := range m {
		if n > bestN {
			best = k
			bestN = n
		}
	}
	return best
}

// maxKeyInt returns the int key with the highest count.
func maxKeyInt(m map[int]int) int {
	best := 0
	bestN := 0
	for k, n := range m {
		if n > bestN {
			best = k
			bestN = n
		}
	}
	return best
}

// formatRAMMiB converts MiB to human-readable (e.g. 4096 → "4G", 512 → "512M").
func formatRAMMiB(mib int) string {
	if mib > 0 && mib%1024 == 0 {
		return fmt.Sprintf("%dG", mib/1024)
	}
	return fmt.Sprintf("%dM", mib)
}

func (h *Handler) teamDestroy(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: ssh <host> team destroy <team-name>\n")
		return 1
	}
	teamName := args[0]

	// Find all VMs for this team by name prefix
	resp, err := h.get("/api/vms")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing VMs: %v\n", err)
		return 1
	}

	var vms []map[string]interface{}
	json.Unmarshal(resp, &vms)

	var targets []string
	prefix := teamName + "-"
	for _, vm := range vms {
		name, _ := vm["name"].(string)
		if strings.HasPrefix(name, prefix) {
			targets = append(targets, name)
		}
	}

	if len(targets) == 0 {
		fmt.Printf("No VMs found for team %q\n", teamName)
		return 0
	}

	// Destroy highest-indexed first
	sort.Sort(sort.Reverse(sort.StringSlice(targets)))

	fmt.Printf("Destroying %d VMs for team %q:\n\n", len(targets), teamName)
	destroyed := 0
	for _, name := range targets {
		fmt.Printf("  destroy  %s ...", name)
		_, err := h.delete("/api/vms/" + name)
		if err != nil {
			fmt.Printf(" FAILED: %v\n", err)
			continue
		}
		fmt.Printf(" ok\n")
		destroyed++
	}

	fmt.Printf("\nDestroyed %d/%d VMs\n", destroyed, len(targets))
	return 0
}

func (h *Handler) teamList(_ []string) int {
	resp, err := h.get("/api/vms")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing VMs: %v\n", err)
		return 1
	}

	var vms []map[string]interface{}
	json.Unmarshal(resp, &vms)

	// Group VMs by team name prefix heuristic:
	// team VMs are named {team}-{agent}-{N}
	teams := make(map[string][]string)
	for _, vm := range vms {
		name, _ := vm["name"].(string)
		parts := strings.Split(name, "-")
		if len(parts) >= 3 {
			// Check if last part is a number (replica index)
			last := parts[len(parts)-1]
			isNum := true
			for _, c := range last {
				if c < '0' || c > '9' {
					isNum = false
					break
				}
			}
			if isNum && len(last) > 0 {
				// Team name is everything before the last two segments
				teamName := strings.Join(parts[:len(parts)-2], "-")
				teams[teamName] = append(teams[teamName], name)
			}
		}
	}

	if len(teams) == 0 {
		fmt.Println("No teams found")
		return 0
	}

	// Sort team names
	var teamNames []string
	for name := range teams {
		teamNames = append(teamNames, name)
	}
	sort.Strings(teamNames)

	fmt.Printf("%-30s  %s\n", "TEAM", "VMs")
	for _, name := range teamNames {
		fmt.Printf("%-30s  %d\n", name, len(teams[name]))
	}
	return 0
}

func (h *Handler) teamMetrics(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: ssh <host> team metrics <team-name>\n")
		return 1
	}
	teamName := args[0]

	// Fetch metrics
	resp, err := h.get("/api/metrics")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching metrics: %v\n", err)
		return 1
	}

	var allMetrics []map[string]interface{}
	json.Unmarshal(resp, &allMetrics)

	// Filter to this team
	prefix := teamName + "-"
	var teamMetrics []map[string]interface{}
	var totalCost float64
	var totalOutput int
	for _, m := range allMetrics {
		name, _ := m["name"].(string)
		if strings.HasPrefix(name, prefix) {
			teamMetrics = append(teamMetrics, m)
			if cost, ok := m["estimated_cost_usd"].(float64); ok {
				totalCost += cost
			}
			if out, ok := m["output_tokens"].(float64); ok {
				totalOutput += int(out)
			}
		}
	}

	// Also fetch activity for utilization info
	actResp, _ := h.get("/api/tapegun/activity")
	actByName := make(map[string]string)
	if actResp != nil {
		var acts []map[string]interface{}
		json.Unmarshal(actResp, &acts)
		for _, a := range acts {
			name, _ := a["name"].(string)
			status := "unknown"
			if act, ok := a["activity"].(map[string]interface{}); ok {
				if s, ok := act["status"].(string); ok {
					status = s
				}
			}
			actByName[name] = status
		}
	}

	fmt.Printf("Team: %s | Cost: $%.2f | Output tokens: %s\n\n",
		teamName, totalCost, formatTokens(totalOutput))

	fmt.Printf("%-30s  %-8s  %-10s  %-12s  %-8s  %s\n",
		"AGENT", "STATUS", "API CALLS", "TOKENS(OUT)", "COST", "MODEL")

	if len(teamMetrics) == 0 {
		// No metrics yet — show VMs from activity data
		for name, status := range actByName {
			if strings.HasPrefix(name, prefix) {
				fmt.Printf("%-30s  %-8s  %-10s  %-12s  %-8s  %s\n",
					name, status, "-", "-", "-", "-")
			}
		}
		if totalOutput == 0 {
			fmt.Println("\nNo token metrics collected yet. Metrics update every 5 minutes.")
		}
	} else {
		for _, m := range teamMetrics {
			name, _ := m["name"].(string)
			apiCalls, _ := m["api_calls"].(float64)
			outTokens, _ := m["output_tokens"].(float64)
			cost, _ := m["estimated_cost_usd"].(float64)
			model, _ := m["model"].(string)
			status := actByName[name]
			if status == "" {
				status = "unknown"
			}
			if model == "" {
				model = "-"
			}

			fmt.Printf("%-30s  %-8s  %-10.0f  %-12s  $%-7.2f  %s\n",
				name, status, apiCalls, formatTokens(int(outTokens)), cost, model)
		}
	}

	return 0
}

func formatTokens(n int) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

func (h *Handler) teamStatus(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: ssh <host> team status <team-name>\n")
		return 1
	}
	teamName := args[0]

	resp, err := h.get("/api/vms")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing VMs: %v\n", err)
		return 1
	}

	var vms []map[string]interface{}
	json.Unmarshal(resp, &vms)

	prefix := teamName + "-"
	var teamVMs []map[string]interface{}
	for _, vm := range vms {
		name, _ := vm["name"].(string)
		if strings.HasPrefix(name, prefix) {
			teamVMs = append(teamVMs, vm)
		}
	}

	if len(teamVMs) == 0 {
		fmt.Printf("No VMs found for team %q\n", teamName)
		return 0
	}

	fmt.Printf("Team: %s (%d VMs)\n\n", teamName, len(teamVMs))
	fmt.Printf("%-35s  %-12s  %-12s  %-6s  %-5s  %s\n",
		"NAME", "STATUS", "NODE", "TYPE", "VCPU", "RAM")
	for _, vm := range teamVMs {
		name, _ := vm["name"].(string)
		status, _ := vm["status"].(string)
		nodeName, _ := vm["node_name"].(string)
		vmType, _ := vm["type"].(string)
		vcpu, _ := vm["vcpu"].(float64)
		ramMIB, _ := vm["ram_mib"].(float64)

		if vmType == "" || vmType == "firecracker" {
			vmType = "fc"
		}
		if status == "" {
			status = "unknown"
		}

		fmt.Printf("%-35s  %-12s  %-12s  %-6s  %-5.0f  %s\n",
			name, status, nodeName, vmType, vcpu, formatRAM(ramMIB))
	}
	return 0
}

// --- Team collaboration commands ---

func (h *Handler) teamMsg(args []string) int {
	if len(args) < 2 {
		fmt.Println("Usage: ssh <host> team message <team-name> <message...>")
		fmt.Println("       ssh <host> team message <team-name> --to <agent> <message...>")
		return 1
	}

	teamName := args[0]
	var to, msgType, subject string
	var msgParts []string

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--to":
			if i+1 < len(args) {
				to = args[i+1]
				i++
			}
		case "--type":
			if i+1 < len(args) {
				msgType = args[i+1]
				i++
			}
		case "--subject":
			if i+1 < len(args) {
				subject = args[i+1]
				i++
			}
		default:
			msgParts = append(msgParts, args[i])
		}
	}

	if len(msgParts) == 0 {
		fmt.Println("Error: message body is required")
		return 1
	}

	if msgType == "" {
		msgType = "notification"
	}

	body := map[string]interface{}{
		"from":    "operator",
		"to":      to,
		"type":    msgType,
		"subject": subject,
		"body":    strings.Join(msgParts, " "),
	}
	bodyJSON, _ := json.Marshal(body)

	resp, err := h.post("/api/team/"+teamName+"/message", bodyJSON)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}
	var result struct {
		ID string `json:"id"`
	}
	json.Unmarshal(resp, &result)

	if to != "" {
		fmt.Printf("  Sent to %s (id: %s)\n", to, result.ID)
	} else {
		fmt.Printf("  Broadcast to team %s (id: %s)\n", teamName, result.ID)
	}
	return 0
}

func (h *Handler) teamFilesCmd(args []string) int {
	if len(args) < 1 {
		fmt.Println("Usage: ssh <host> team files <team-name>")
		return 1
	}

	body, err := h.get("/api/team/" + args[0] + "/files")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}

	var reports []struct {
		Agent  string   `json:"agent"`
		Branch string   `json:"branch"`
		Files  []string `json:"files"`
	}
	json.Unmarshal(body, &reports)

	if len(reports) == 0 {
		fmt.Println("  No file reports for this team")
		return 0
	}

	for _, r := range reports {
		fmt.Printf("  %s (branch: %s)\n", r.Agent, r.Branch)
		for _, f := range r.Files {
			fmt.Printf("    - %s\n", f)
		}
	}
	return 0
}

func (h *Handler) teamConflictsCmd(args []string) int {
	if len(args) < 1 {
		fmt.Println("Usage: ssh <host> team conflicts <team-name>")
		return 1
	}

	body, err := h.get("/api/team/" + args[0] + "/conflicts")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}

	var conflicts []struct {
		File    string `json:"file"`
		AgentA  string `json:"agent_a"`
		BranchA string `json:"branch_a"`
		AgentB  string `json:"agent_b"`
		BranchB string `json:"branch_b"`
	}
	json.Unmarshal(body, &conflicts)

	if len(conflicts) == 0 {
		fmt.Println("  No file conflicts detected")
		return 0
	}

	fmt.Printf("  %d conflict(s) detected:\n", len(conflicts))
	for _, c := range conflicts {
		fmt.Printf("  %s\n", c.File)
		fmt.Printf("    %s (branch: %s)\n", c.AgentA, c.BranchA)
		fmt.Printf("    %s (branch: %s)\n", c.AgentB, c.BranchB)
	}
	return 0
}

func (h *Handler) teamScratchpadCmd(args []string) int {
	if len(args) < 1 {
		fmt.Println("Usage: ssh <host> team scratchpad <team-name> [--add <type> <message>]")
		return 1
	}

	teamName := args[0]

	// Check if adding an entry
	if len(args) > 1 && args[1] == "--add" {
		if len(args) < 4 {
			fmt.Println("Usage: ssh <host> team scratchpad <team-name> --add <type> <message>")
			fmt.Println("  Types: decision, discovery, warning, blocker")
			return 1
		}
		entryType := args[2]
		entryBody := strings.Join(args[3:], " ")

		body := map[string]interface{}{
			"from": "operator",
			"type": entryType,
			"body": entryBody,
		}
		bodyJSON, _ := json.Marshal(body)

		_, err := h.post("/api/team/"+teamName+"/scratchpad", bodyJSON)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		fmt.Printf("  Added %s entry to %s scratchpad\n", entryType, teamName)
		return 0
	}

	// List scratchpad
	body, err := h.get("/api/team/" + teamName + "/scratchpad")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 1
	}

	var entries []struct {
		From      string `json:"from"`
		Type      string `json:"type"`
		Body      string `json:"body"`
		Timestamp string `json:"timestamp"`
	}
	json.Unmarshal(body, &entries)

	if len(entries) == 0 {
		fmt.Println("  Scratchpad is empty")
		return 0
	}

	for _, e := range entries {
		fmt.Printf("  [%s] %s (%s): %s\n", e.Type, e.From, e.Timestamp, e.Body)
	}
	return 0
}

// v1 queue helpers removed — v2 implementations below (cmdQueueList, cmdQueueAdd, etc.)

func (h *Handler) printHelp() {
	fmt.Print(`Boxcutter — ephemeral dev environments

Commands:
  new [options]           Create and start a new VM
    --name <name>           Custom VM name (default: auto-generated)
    --type <type>           VM type: firecracker (default) or qemu
    --desc <text>           Description of what this VM is for
    --clone <repo>          Clone repo on creation (repeatable)
    --vcpu <N>              CPU cores (default: 2)
    --ram <MiB>             RAM in MiB (default: 2048)
    --disk <size>           Disk size (default: 50G)
    --mode normal|paranoid  Network mode (default: normal)
    --node <node-id>        Pin to specific node
    --ts-authkey <key>      Tailscale auth key (default: node's shared key)
  list                    List all VMs (shows type, description)
  logs <name> [--lines N] Show VM console/system logs (default: last 100 lines)
  describe <name> <text>  Set or update a VM's description
  destroy <name>          Destroy a VM
  stop <name>             Stop a running VM
  start <name>            Start a stopped VM
  cp <name> [new-name]    Copy a VM (clone its disk)
  exec <name> <command>   Run a command on a VM and return output
  authorize <name>        Grant a VM access to boxcutter commands
  repos list <name>       List GitHub repos for a VM
  repos add <name> <repo> Add a repo to VM's GitHub policy
  repos remove <name> <repo>
                          Remove a repo from VM's GitHub policy
  projects list <name>    List GitHub projects for a VM
  projects add <name> <p> Add a project to VM's GitHub policy
  projects remove <name> <p>
                          Remove a project from VM's policy
  images                  List golden images across all nodes
  golden set-head <ver>   Set golden image head version (nodes pull via MQTT)
  status                  Cluster capacity summary
  nodes                   List all nodes
  adduser <github-user>   Add SSH keys from GitHub (for new VMs)
  removeuser <github-user>
                          Remove SSH keys for a user
  keys                    List all configured SSH keys
  tapegun activity [name] Monitor VM activity (all or specific)
  tapegun send <name> <msg>
                          Send a message to a VM's inbox
  tapegun sendkeys [--no-enter] <name> <cmd>
                          Inject a command into a VM's tmux pane (auto-submits with Enter)
  tapegun broadcast <msg> Broadcast to all running VMs
  exec <name> <cmd>       Run a command on a VM
  cp-to <name> <src> <dst>
                          Copy a file into a VM (use - for stdin)
  cp-from <name> <src> <dst>
                          Copy a file out of a VM (use - for stdout)
  team apply -f -         Create/update team from YAML (stdin)
  team diff -f -          Show what apply would change (dry-run)
  team export <name>      Export running team as YAML
  team destroy <name>     Destroy all VMs for a team
  team list               List active teams
  team metrics <name>     Show token usage and cost per agent
  team status <name>      Show status of team VMs
  queue list [--status s] List work queue items (filter: queued|assigned|completed|failed)
  queue add --title <t>   Add item manually [--source ref] [--priority N] [--team name]
  queue stats             Show queue statistics
  queue complete <id>     Mark item complete
  queue reassign <id>     Return item to queue
  queue sync              Trigger GitHub issue sync
  msg topics              List messaging topics
  msg queues              List messaging queues (with depth)
  msg publish <topic> <message>
                          Publish to a topic (fans out to subscribed queues)
  msg send <agent> <message>
                          Send directly to an agent's inbox queue
  msg read <queue>        Read messages from a queue
  msg subscribe <topic> <queue>
                          Subscribe a queue to a topic
  msg subscriptions       List all subscriptions
  msg inspect <queue>     Show queue depth
  help                    Show this help

Usage: ssh <host> <command> [args]
`)
}

// --- Messaging commands (#190) ---

// formatRAM formats RAM in MiB to a human-readable string (e.g., 512M, 2G).
func formatRAM(mib float64) string {
	if mib < 1024 {
		return fmt.Sprintf("%.0fM", mib)
	}
	gb := mib / 1024
	if gb == float64(int(gb)) {
		return fmt.Sprintf("%.0fG", gb)
	}
	return fmt.Sprintf("%.1fG", gb)
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

// --- Queue commands ---


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
