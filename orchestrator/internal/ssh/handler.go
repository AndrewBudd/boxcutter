package ssh

import (
	"bytes"
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
	case "status":
		return h.teamStatus(args[1:])
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

	// Fetch existing VMs to determine create vs noop
	existingVMs := make(map[string]bool)
	resp, err := h.get("/api/vms")
	if err == nil {
		var vms []map[string]interface{}
		json.Unmarshal(resp, &vms)
		for _, vm := range vms {
			if name, ok := vm["name"].(string); ok {
				existingVMs[name] = true
			}
		}
	}

	// Compute diff
	var toCreate []team.ResolvedAgent
	var existing []string
	for _, a := range agents {
		if existingVMs[a.VMName] {
			existing = append(existing, a.VMName)
		} else {
			toCreate = append(toCreate, a)
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
			// Check if this VM belongs to this team by name prefix
			if strings.HasPrefix(name, ts.Metadata.Name+"-") && !wantVMs[name] {
				toDestroy = append(toDestroy, name)
			}
		}
	}
	// Destroy highest-indexed first
	sort.Sort(sort.Reverse(sort.StringSlice(toDestroy)))

	// Report plan
	for _, name := range existing {
		fmt.Printf("  ok       %s\n", name)
	}

	// Create new VMs
	created := 0
	for _, a := range toCreate {
		body := map[string]interface{}{
			"name":    a.VMName,
			"type":    a.Type,
			"vcpu":    a.VCPU,
			"ram_mib": a.RAMMiB(),
			"disk":    a.Disk,
			"mode":    a.Mode,
			"labels":  a.Labels(ts.Metadata.Name),
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
		_, err := h.postStream("/api/vms", body, func(evt map[string]interface{}) {
			// suppress progress for batch creation
		})
		if err != nil {
			fmt.Printf(" FAILED: %v\n", err)
			continue
		}
		fmt.Printf(" ok\n")
		created++
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

	fmt.Printf("\nResult: %d created, %d existing, %d destroyed\n", created, len(existing), destroyed)
	return 0
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
  team status <name>      Show status of team VMs
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

func (h *Handler) patch(path string, data []byte) ([]byte, error) {
	req, _ := http.NewRequest("PATCH", h.apiBase+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
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
