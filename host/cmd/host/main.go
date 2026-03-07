package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AndrewBudd/boxcutter/host/internal/bridge"
	"github.com/AndrewBudd/boxcutter/host/internal/cluster"
	"github.com/AndrewBudd/boxcutter/host/internal/oci"
	"github.com/AndrewBudd/boxcutter/host/internal/qemu"
)

// Config for the host control plane, loaded from boxcutter.env equivalents.
type HostConfig struct {
	RepoDir       string
	ImagesDir     string
	HostNIC       string
	BridgeDevice  string
	BridgeIP      string
	BridgeCIDR    string
	OrchestratorIP string
	OrchestratorMAC string
	OrchestratorTAP string
	OrchestratorVCPU int
	OrchestratorRAM string
	OrchestratorDisk string
	NodeVCPU      int
	NodeRAM       string
	NodeDisk      string
	NodeSubnet    string
	NodeIPOffset  int
	StatePath     string
	SocketPath    string

	// Auto-scaling
	ScalePollInterval   time.Duration
	HealthPollInterval  time.Duration
	ScaleUpThresholdPct int // Scale up when VM capacity used > this %
	MinFreeMemoryMB     int // Hard floor: never scale up if host has less than this free

	// OCI image distribution
	OCIRegistry   string // OCI registry (default: ghcr.io)
	OCIRepository string // Repository path (default: AndrewBudd/boxcutter)
}

func defaultConfig() HostConfig {
	// Find repo dir: check env, then try relative to binary, then home
	repoDir := os.Getenv("BOXCUTTER_REPO")
	if repoDir == "" {
		// If binary is at host/boxcutter-host or host/cmd/host/host, walk up
		exe, _ := os.Executable()
		if exe != "" {
			dir := exe
			for i := 0; i < 4; i++ {
				dir = filepath.Dir(dir)
				if fileExists(filepath.Join(dir, "host", "boxcutter.env")) {
					repoDir = dir
					break
				}
			}
		}
	}
	if repoDir == "" {
		home, _ := os.UserHomeDir()
		repoDir = home + "/boxcutter"
	}
	return HostConfig{
		RepoDir:            repoDir,
		ImagesDir:          repoDir + "/.images",
		HostNIC:            "enp34s0",
		BridgeDevice:       "br-boxcutter",
		BridgeIP:           "192.168.50.1",
		BridgeCIDR:         "24",
		OrchestratorIP:     "192.168.50.2",
		OrchestratorMAC:    "52:54:00:00:00:02",
		OrchestratorTAP:    "tap-orch",
		OrchestratorVCPU:   2,
		OrchestratorRAM:    "4G",
		OrchestratorDisk:   "20G",
		NodeVCPU:           6,
		NodeRAM:            "12G",
		NodeDisk:           "150G",
		NodeSubnet:         "192.168.50",
		NodeIPOffset:       2,
		StatePath:          "/var/lib/boxcutter/cluster.json",
		SocketPath:         "/run/boxcutter-host.sock",
		ScalePollInterval:   30 * time.Second,
		HealthPollInterval:  10 * time.Second,
		ScaleUpThresholdPct: 80,
		MinFreeMemoryMB:     8192, // 8GB — never launch a node if host has less than this free
		OCIRegistry:         oci.DefaultRegistry,
		OCIRepository:       oci.DefaultRepository,
	}
}

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "run":
		runDaemon()
	case "status":
		cliStatus()
	case "bootstrap":
		runBootstrap()
	case "pull":
		cliPull()
	case "upgrade":
		cliUpgrade()
	case "version":
		cliVersion()
	case "build-image":
		cliBuildImage()
	default:
		fmt.Fprintf(os.Stderr, "Usage: boxcutter-host <run|status|bootstrap|pull|upgrade|version|build-image>\n")
		os.Exit(1)
	}
}

func runDaemon() {
	cfg := defaultConfig()

	log.Println("boxcutter-host starting...")

	// 1. Set up bridge + NAT
	if err := bridge.Setup(bridge.Config{
		BridgeDevice: cfg.BridgeDevice,
		BridgeIP:     cfg.BridgeIP,
		BridgeCIDR:   cfg.BridgeCIDR,
		HostNIC:      cfg.HostNIC,
	}); err != nil {
		log.Fatalf("Bridge setup failed: %v", err)
	}

	// 2. Load cluster state
	state, err := cluster.Load(cfg.StatePath)
	if err != nil {
		log.Fatalf("Loading cluster state: %v", err)
	}

	// 3. Boot recovery — launch all VMs from state
	bootRecover(cfg, state)

	// 4. Start unix socket API
	go startAPI(cfg, state)

	// 5. Start health monitor
	go healthLoop(cfg, state)

	// 6. Start auto-scaler
	go autoScaleLoop(cfg, state)

	log.Println("boxcutter-host ready")

	// Wait for shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("boxcutter-host shutting down")
}

func bootRecover(cfg HostConfig, state *cluster.State) {
	currentUser, _ := user.Current()
	username := "root"
	if currentUser != nil {
		username = currentUser.Username
	}

	// Launch orchestrator
	if state.Orchestrator != nil {
		orch := state.Orchestrator
		if err := bridge.EnsureTAP(orch.TAP, cfg.BridgeDevice, username); err != nil {
			log.Printf("WARNING: orchestrator TAP: %v", err)
		}
		pid, err := qemu.Launch(qemu.VMConfig{
			Name: orch.ID,
			VCPU: orch.VCPU,
			RAM:  orch.RAM,
			Disk: orch.Disk,
			ISO:  orch.ISO,
			TAP:  orch.TAP,
			MAC:  orch.MAC,
		}, cfg.ImagesDir)
		if err != nil {
			log.Printf("WARNING: orchestrator launch failed: %v", err)
		} else {
			state.SetPID(orch.ID, pid)
		}
	}

	// Launch nodes
	for _, node := range state.Nodes {
		if err := bridge.EnsureTAP(node.TAP, cfg.BridgeDevice, username); err != nil {
			log.Printf("WARNING: node %s TAP: %v", node.ID, err)
		}
		pid, err := qemu.Launch(qemu.VMConfig{
			Name: node.ID,
			VCPU: node.VCPU,
			RAM:  node.RAM,
			Disk: node.Disk,
			ISO:  node.ISO,
			TAP:  node.TAP,
			MAC:  node.MAC,
		}, cfg.ImagesDir)
		if err != nil {
			log.Printf("WARNING: node %s launch failed: %v", node.ID, err)
		} else {
			state.SetPID(node.ID, pid)
		}
	}

	state.Save()
}

// healthLoop periodically checks that all VMs are running.
func healthLoop(cfg HostConfig, state *cluster.State) {
	ticker := time.NewTicker(cfg.HealthPollInterval)
	defer ticker.Stop()

	for range ticker.C {
		// Check orchestrator
		if state.Orchestrator != nil {
			if !qemu.IsRunning(state.Orchestrator.PID) {
				log.Printf("ALERT: orchestrator not running, restarting...")
				currentUser, _ := user.Current()
				username := "root"
				if currentUser != nil {
					username = currentUser.Username
				}
				bridge.EnsureTAP(state.Orchestrator.TAP, cfg.BridgeDevice, username)
				pid, err := qemu.Launch(qemu.VMConfig{
					Name: state.Orchestrator.ID,
					VCPU: state.Orchestrator.VCPU,
					RAM:  state.Orchestrator.RAM,
					Disk: state.Orchestrator.Disk,
					ISO:  state.Orchestrator.ISO,
					TAP:  state.Orchestrator.TAP,
					MAC:  state.Orchestrator.MAC,
				}, cfg.ImagesDir)
				if err != nil {
					log.Printf("orchestrator restart failed: %v", err)
				} else {
					state.SetPID(state.Orchestrator.ID, pid)
					state.Save()
				}
			}
		}

		// Check nodes
		for _, node := range state.Nodes {
			if !qemu.IsRunning(node.PID) {
				log.Printf("ALERT: node %s not running, restarting...", node.ID)
				currentUser, _ := user.Current()
				username := "root"
				if currentUser != nil {
					username = currentUser.Username
				}
				bridge.EnsureTAP(node.TAP, cfg.BridgeDevice, username)
				pid, err := qemu.Launch(qemu.VMConfig{
					Name: node.ID,
					VCPU: node.VCPU,
					RAM:  node.RAM,
					Disk: node.Disk,
					ISO:  node.ISO,
					TAP:  node.TAP,
					MAC:  node.MAC,
				}, cfg.ImagesDir)
				if err != nil {
					log.Printf("node %s restart failed: %v", node.ID, err)
				} else {
					state.SetPID(node.ID, pid)
					state.Save()
				}
			}
		}
	}
}

// autoScaleLoop polls nodes for capacity and scales up/down.
func autoScaleLoop(cfg HostConfig, state *cluster.State) {
	// Wait for VMs to boot before polling
	time.Sleep(30 * time.Second)

	ticker := time.NewTicker(cfg.ScalePollInterval)
	defer ticker.Stop()

	for range ticker.C {
		if state.NodeCount() == 0 {
			continue
		}

		totalRAM := 0
		usedRAM := 0
		totalVMs := 0

		for _, node := range state.Nodes {
			if !qemu.IsRunning(node.PID) {
				continue
			}
			health := queryNodeHealth(node.BridgeIP)
			if health == nil {
				continue
			}
			if v, ok := health["ram_total_mib"].(float64); ok {
				totalRAM += int(v)
			}
			if v, ok := health["ram_allocated_mib"].(float64); ok {
				usedRAM += int(v)
			}
			if v, ok := health["vms_running"].(float64); ok {
				totalVMs += int(v)
			}
		}

		if totalRAM == 0 {
			continue
		}

		usedPct := (usedRAM * 100) / totalRAM
		log.Printf("Capacity: %d/%d MiB (%d%%), %d VMs across %d nodes",
			usedRAM, totalRAM, usedPct, totalVMs, state.NodeCount())

		if usedPct >= cfg.ScaleUpThresholdPct {
			log.Printf("Capacity above %d%%, checking if scale-up is possible...", cfg.ScaleUpThresholdPct)
			ok, reason := canScaleUp(cfg)
			if ok {
				log.Printf("Scaling up: adding new node...")
				addNode(cfg, state)
			} else {
				log.Printf("Cannot scale up: %s", reason)
			}
		}
	}
}

func queryNodeHealth(bridgeIP string) map[string]interface{} {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:8800/api/health", bridgeIP))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result
}

func canScaleUp(cfg HostConfig) (bool, string) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return false, "cannot read /proc/meminfo"
	}
	var availKB int
	for _, line := range splitLines(string(data)) {
		if len(line) > 13 && line[:13] == "MemAvailable:" {
			fmt.Sscanf(line, "MemAvailable: %d kB", &availKB)
			break
		}
	}
	availMB := availKB / 1024

	// Hard floor: never scale if below minimum free memory
	if availMB < cfg.MinFreeMemoryMB {
		return false, fmt.Sprintf("host has %dMB free, minimum is %dMB", availMB, cfg.MinFreeMemoryMB)
	}

	// Also need enough for the node VM itself (parse NodeRAM like "12G")
	var nodeRAMMB int
	fmt.Sscanf(cfg.NodeRAM, "%dG", &nodeRAMMB)
	nodeRAMMB *= 1024
	if nodeRAMMB == 0 {
		nodeRAMMB = 12 * 1024 // fallback
	}

	// After launching, we must still have MinFreeMemoryMB left
	afterLaunchMB := availMB - nodeRAMMB
	if afterLaunchMB < cfg.MinFreeMemoryMB {
		return false, fmt.Sprintf("after launch would have %dMB free (%dMB available - %dMB node), minimum is %dMB",
			afterLaunchMB, availMB, nodeRAMMB, cfg.MinFreeMemoryMB)
	}

	return true, ""
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func addNode(cfg HostConfig, state *cluster.State) {
	nodeNum := state.NextNodeNum()
	nodeID := fmt.Sprintf("boxcutter-node-%d", nodeNum)
	nodeOctet := cfg.NodeIPOffset + nodeNum
	bridgeIP := fmt.Sprintf("%s.%d", cfg.NodeSubnet, nodeOctet)
	tap := fmt.Sprintf("tap-node%d", nodeNum)
	mac := fmt.Sprintf("52:54:00:00:00:%02x", nodeOctet)
	disk := fmt.Sprintf("%s/%s.qcow2", cfg.ImagesDir, nodeID)
	iso := fmt.Sprintf("%s/%s-cloud-init.iso", cfg.ImagesDir, nodeID)

	// Check if disk exists (node must be provisioned first)
	if _, err := os.Stat(disk); err != nil {
		log.Printf("Node %s disk not found at %s — provision first", nodeID, disk)
		// TODO: auto-provision from repo
		return
	}

	currentUser, _ := user.Current()
	username := "root"
	if currentUser != nil {
		username = currentUser.Username
	}

	if err := bridge.EnsureTAP(tap, cfg.BridgeDevice, username); err != nil {
		log.Printf("Failed to create TAP for %s: %v", nodeID, err)
		return
	}

	pid, err := qemu.Launch(qemu.VMConfig{
		Name: nodeID,
		VCPU: cfg.NodeVCPU,
		RAM:  cfg.NodeRAM,
		Disk: disk,
		ISO:  iso,
		TAP:  tap,
		MAC:  mac,
	}, cfg.ImagesDir)
	if err != nil {
		log.Printf("Failed to launch %s: %v", nodeID, err)
		return
	}

	state.AddNode(cluster.VMEntry{
		ID:       nodeID,
		BridgeIP: bridgeIP,
		Disk:     disk,
		ISO:      iso,
		PID:      pid,
		VCPU:     cfg.NodeVCPU,
		RAM:      cfg.NodeRAM,
		TAP:      tap,
		MAC:      mac,
	})
	state.Save()
	log.Printf("Node %s added (PID %d, IP %s)", nodeID, pid, bridgeIP)
}

// startAPI serves the unix socket control API.
func startAPI(cfg HostConfig, state *cluster.State) {
	os.Remove(cfg.SocketPath)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		status := buildStatus(cfg, state)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})

	mux.HandleFunc("POST /drain/{nodeID}", func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("nodeID")
		go drainNode(cfg, state, nodeID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "draining", "node": nodeID})
	})

	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		log.Fatalf("Unix socket: %v", err)
	}
	os.Chmod(cfg.SocketPath, 0660)

	log.Printf("Control API listening on %s", cfg.SocketPath)
	http.Serve(listener, mux)
}

func buildStatus(cfg HostConfig, state *cluster.State) map[string]interface{} {
	result := map[string]interface{}{}

	if state.Orchestrator != nil {
		orch := state.Orchestrator
		result["orchestrator"] = map[string]interface{}{
			"id":        orch.ID,
			"bridge_ip": orch.BridgeIP,
			"pid":       orch.PID,
			"running":   qemu.IsRunning(orch.PID),
		}
	}

	nodes := []map[string]interface{}{}
	for _, n := range state.Nodes {
		nodeStatus := map[string]interface{}{
			"id":        n.ID,
			"bridge_ip": n.BridgeIP,
			"pid":       n.PID,
			"running":   qemu.IsRunning(n.PID),
		}
		// Try to get health from node agent
		if qemu.IsRunning(n.PID) {
			if health := queryNodeHealth(n.BridgeIP); health != nil {
				nodeStatus["health"] = health
			}
		}
		nodes = append(nodes, nodeStatus)
	}
	result["nodes"] = nodes

	return result
}

// drainNode migrates all Firecrackers off a node, then stops it.
func drainNode(cfg HostConfig, state *cluster.State, nodeID string) {
	node := state.GetNode(nodeID)
	if node == nil {
		log.Printf("Drain: node %s not found", nodeID)
		return
	}

	log.Printf("Draining node %s...", nodeID)

	// Get list of VMs on this node
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:8800/api/vms", node.BridgeIP))
	if err != nil {
		log.Printf("Drain: cannot list VMs on %s: %v", nodeID, err)
		return
	}
	defer resp.Body.Close()

	var vms []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&vms)

	if len(vms) == 0 {
		log.Printf("Drain: %s has no VMs, stopping", nodeID)
		qemu.Stop(nodeID, node.PID)
		state.RemoveNode(nodeID)
		state.Save()
		return
	}

	// Find a target node
	var targetNode *cluster.VMEntry
	for _, n := range state.Nodes {
		if n.ID != nodeID && qemu.IsRunning(n.PID) {
			entry := n
			targetNode = &entry
			break
		}
	}
	if targetNode == nil {
		log.Printf("Drain: no target node available for migration")
		return
	}

	// Migrate each VM
	for _, vm := range vms {
		vmName, ok := vm["name"].(string)
		if !ok {
			continue
		}

		log.Printf("Drain: migrating %s from %s to %s", vmName, nodeID, targetNode.ID)

		migrateReq := map[string]string{
			"target_addr":     fmt.Sprintf("%s:8800", targetNode.BridgeIP),
			"target_bridge_ip": targetNode.BridgeIP,
		}
		data, _ := json.Marshal(migrateReq)

		migrateResp, err := client.Post(
			fmt.Sprintf("http://%s:8800/api/vms/%s/migrate", node.BridgeIP, vmName),
			"application/json",
			jsonReader(data),
		)
		if err != nil {
			log.Printf("Drain: migrate %s failed: %v", vmName, err)
			continue
		}
		migrateResp.Body.Close()

		if migrateResp.StatusCode >= 300 {
			log.Printf("Drain: migrate %s returned %d", vmName, migrateResp.StatusCode)
		} else {
			log.Printf("Drain: %s migrated successfully", vmName)
		}
	}

	// Stop the drained node
	log.Printf("Drain: stopping %s", nodeID)
	qemu.Stop(nodeID, node.PID)
	state.RemoveNode(nodeID)
	state.Save()
	log.Printf("Drain: %s complete", nodeID)
}

func jsonReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}

// cliStatus connects to the unix socket and prints status.
func cliStatus() {
	cfg := defaultConfig()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", cfg.SocketPath)
			},
		},
	}

	resp, err := client.Get("http://localhost/status")
	if err != nil {
		// Fallback: read cluster.json directly
		state, loadErr := cluster.Load(cfg.StatePath)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "Cannot connect to boxcutter-host and no cluster.json found\n")
			os.Exit(1)
		}
		printOfflineStatus(state)
		return
	}
	defer resp.Body.Close()

	var status map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&status)

	out, _ := json.MarshalIndent(status, "", "  ")
	fmt.Println(string(out))
}

func printOfflineStatus(state *cluster.State) {
	fmt.Println("boxcutter-host not running (showing last known state)")
	fmt.Println()

	if state.Orchestrator != nil {
		o := state.Orchestrator
		running := qemu.IsRunning(o.PID)
		status := "stopped"
		if running {
			status = "running"
		}
		fmt.Printf("Orchestrator: %s (IP %s, PID %d, %s)\n", o.ID, o.BridgeIP, o.PID, status)
	}

	for _, n := range state.Nodes {
		running := qemu.IsRunning(n.PID)
		status := "stopped"
		if running {
			status = "running"
		}
		fmt.Printf("Node: %s (IP %s, PID %d, %s)\n", n.ID, n.BridgeIP, n.PID, status)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// cliPull downloads a pre-built VM image from an OCI registry.
//
//	boxcutter-host pull <node|orchestrator|golden> [--tag TAG]
func cliPull() {
	cfg := defaultConfig()
	vmType, tag := parsePullArgs()

	fmt.Printf("Pulling %s image (tag: %s)...\n", vmType, tag)
	fmt.Printf("  Registry: %s/%s/%s\n", cfg.OCIRegistry, cfg.OCIRepository, vmType)

	ctx := context.Background()

	meta, outputFile, err := oci.Pull(ctx, oci.PullOptions{
		Registry:   cfg.OCIRegistry,
		Repository: cfg.OCIRepository,
		VMType:     vmType,
		Tag:        tag,
		OutputDir:  cfg.ImagesDir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Pull failed: %v\n", err)
		os.Exit(1)
	}

	// Decompress the zstd file
	var ext string
	if vmType == "golden" {
		ext = ".ext4"
	} else {
		ext = ".qcow2"
	}
	baseName := fmt.Sprintf("%s-base%s", vmType, ext)
	basePath := filepath.Join(cfg.ImagesDir, baseName)

	if outputFile != "" && filepath.Ext(outputFile) == ".zst" {
		fmt.Printf("  Decompressing %s...\n", filepath.Base(outputFile))
		if err := decompressZstd(outputFile, basePath); err != nil {
			fmt.Fprintf(os.Stderr, "Decompression failed: %v\n", err)
			os.Exit(1)
		}
		os.Remove(outputFile)
	} else if outputFile != "" {
		os.Rename(outputFile, basePath)
	}

	fmt.Printf("\nPull complete!\n")
	fmt.Printf("  Base image: %s\n", basePath)
	if meta.Version != "" {
		fmt.Printf("  Version: %s\n", meta.Version)
	}
	if meta.Commit != "" {
		fmt.Printf("  Commit: %s\n", meta.Commit)
	}
	fmt.Printf("  Digest: %s\n", meta.Digest)

	if vmType == "golden" {
		fmt.Printf("\nTo deploy this golden image to nodes, copy it to /var/lib/boxcutter/golden/ on each node.\n")
	} else {
		fmt.Printf("\nTo provision a VM from this image:\n")
		fmt.Printf("  bash host/provision.sh %s --from-image\n", vmType)
	}
}

// cliUpgrade pulls a new image and performs a rolling upgrade.
//
//	boxcutter-host upgrade <node|orchestrator|all> [--tag TAG]
func cliUpgrade() {
	cfg := defaultConfig()
	vmType, tag := parsePullArgs()

	state, err := cluster.Load(cfg.StatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Loading cluster state: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// For "all", pull both
	types := []string{vmType}
	if vmType == "all" {
		types = []string{"node", "orchestrator"}
	}

	for _, t := range types {
		fmt.Printf("Pulling %s image (tag: %s)...\n", t, tag)
		meta, outputFile, err := oci.Pull(ctx, oci.PullOptions{
			Registry:   cfg.OCIRegistry,
			Repository: cfg.OCIRepository,
			VMType:     t,
			Tag:        tag,
			OutputDir:  cfg.ImagesDir,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Pull failed for %s: %v\n", t, err)
			os.Exit(1)
		}

		baseName := fmt.Sprintf("%s-base.qcow2", t)
		basePath := filepath.Join(cfg.ImagesDir, baseName)
		if outputFile != "" && filepath.Ext(outputFile) == ".zst" {
			fmt.Printf("  Decompressing...\n")
			if err := decompressZstd(outputFile, basePath); err != nil {
				fmt.Fprintf(os.Stderr, "Decompression failed: %v\n", err)
				os.Exit(1)
			}
			os.Remove(outputFile)
		} else if outputFile != "" {
			os.Rename(outputFile, basePath)
		}

		switch t {
		case "node":
			upgradeNodes(cfg, state, basePath, meta)
		case "orchestrator":
			upgradeOrchestrator(cfg, state, basePath, meta)
		}
	}
}

func upgradeNodes(cfg HostConfig, state *cluster.State, basePath string, meta *oci.ImageMetadata) {
	fmt.Printf("\n--- Upgrading nodes ---\n")

	for _, oldNode := range state.Nodes {
		fmt.Printf("Upgrading %s...\n", oldNode.ID)

		newNum := state.NextNodeNum()
		newID := fmt.Sprintf("boxcutter-node-%d", newNum)
		newOctet := cfg.NodeIPOffset + newNum
		newBridgeIP := fmt.Sprintf("%s.%d", cfg.NodeSubnet, newOctet)
		newTAP := fmt.Sprintf("tap-node%d", newNum)
		newMAC := fmt.Sprintf("52:54:00:00:00:%02x", newOctet)
		newDisk := fmt.Sprintf("%s/%s.qcow2", cfg.ImagesDir, newID)
		newISO := fmt.Sprintf("%s/%s-cloud-init.iso", cfg.ImagesDir, newID)

		fmt.Printf("  Creating disk from base image for %s...\n", newID)
		if err := createCOWDisk(basePath, newDisk, cfg.NodeDisk); err != nil {
			log.Printf("Failed to create disk for %s: %v", newID, err)
			continue
		}

		if !fileExists(newISO) {
			fmt.Printf("  Cloud-init ISO not found: %s\n", newISO)
			fmt.Printf("  Generate with: bash host/provision.sh node %s --from-image\n", newID)
			fmt.Printf("  Skipping %s upgrade.\n", oldNode.ID)
			os.Remove(newDisk)
			continue
		}

		currentUser, _ := user.Current()
		username := "root"
		if currentUser != nil {
			username = currentUser.Username
		}

		if err := bridge.EnsureTAP(newTAP, cfg.BridgeDevice, username); err != nil {
			log.Printf("Failed to create TAP for %s: %v", newID, err)
			continue
		}

		pid, err := qemu.Launch(qemu.VMConfig{
			Name: newID,
			VCPU: cfg.NodeVCPU,
			RAM:  cfg.NodeRAM,
			Disk: newDisk,
			ISO:  newISO,
			TAP:  newTAP,
			MAC:  newMAC,
		}, cfg.ImagesDir)
		if err != nil {
			log.Printf("Failed to launch %s: %v", newID, err)
			continue
		}

		state.AddNode(cluster.VMEntry{
			ID:           newID,
			BridgeIP:     newBridgeIP,
			Disk:         newDisk,
			ISO:          newISO,
			PID:          pid,
			VCPU:         cfg.NodeVCPU,
			RAM:          cfg.NodeRAM,
			TAP:          newTAP,
			MAC:          newMAC,
			ImageVersion: meta.Version,
			ImageCommit:  meta.Commit,
			ImageDigest:  meta.Digest,
		})
		state.Save()

		fmt.Printf("  New node %s launched (PID %d). Waiting for health...\n", newID, pid)

		if !waitForNodeHealth(newBridgeIP, 120*time.Second) {
			log.Printf("New node %s did not become healthy, skipping drain of %s", newID, oldNode.ID)
			continue
		}

		fmt.Printf("  Draining old node %s...\n", oldNode.ID)
		drainNode(cfg, state, oldNode.ID)
		fmt.Printf("  Node %s upgraded to %s\n", oldNode.ID, newID)
	}
}

func upgradeOrchestrator(cfg HostConfig, state *cluster.State, basePath string, meta *oci.ImageMetadata) {
	fmt.Printf("\n--- Upgrading orchestrator ---\n")

	if state.Orchestrator == nil {
		fmt.Println("No orchestrator to upgrade.")
		return
	}

	orchDisk := fmt.Sprintf("%s/orchestrator.qcow2", cfg.ImagesDir)
	orchISO := fmt.Sprintf("%s/orchestrator-cloud-init.iso", cfg.ImagesDir)

	if !fileExists(orchISO) {
		fmt.Printf("Cloud-init ISO not found: %s\n", orchISO)
		fmt.Printf("Generate with: bash host/provision.sh orchestrator --from-image\n")
		return
	}

	fmt.Println("  Stopping old orchestrator...")
	qemu.Stop("orchestrator", state.Orchestrator.PID)

	orchDiskNew := orchDisk + ".new"
	if err := createCOWDisk(basePath, orchDiskNew, cfg.OrchestratorDisk); err != nil {
		log.Printf("Failed to create orchestrator disk: %v", err)
		return
	}

	os.Rename(orchDisk, orchDisk+".bak")
	os.Rename(orchDiskNew, orchDisk)

	currentUser, _ := user.Current()
	username := "root"
	if currentUser != nil {
		username = currentUser.Username
	}

	bridge.EnsureTAP(cfg.OrchestratorTAP, cfg.BridgeDevice, username)
	pid, err := qemu.Launch(qemu.VMConfig{
		Name: "orchestrator",
		VCPU: cfg.OrchestratorVCPU,
		RAM:  cfg.OrchestratorRAM,
		Disk: orchDisk,
		ISO:  orchISO,
		TAP:  cfg.OrchestratorTAP,
		MAC:  cfg.OrchestratorMAC,
	}, cfg.ImagesDir)
	if err != nil {
		log.Printf("Orchestrator launch failed: %v", err)
		os.Rename(orchDisk+".bak", orchDisk)
		return
	}

	state.SetOrchestrator(cluster.VMEntry{
		ID:           "orchestrator",
		BridgeIP:     cfg.OrchestratorIP,
		Disk:         orchDisk,
		ISO:          orchISO,
		PID:          pid,
		VCPU:         cfg.OrchestratorVCPU,
		RAM:          cfg.OrchestratorRAM,
		TAP:          cfg.OrchestratorTAP,
		MAC:          cfg.OrchestratorMAC,
		ImageVersion: meta.Version,
		ImageCommit:  meta.Commit,
		ImageDigest:  meta.Digest,
	})
	state.Save()

	os.Remove(orchDisk + ".bak")
	fmt.Printf("  Orchestrator upgraded (PID %d)\n", pid)
}

// cliVersion shows the current and latest available image versions.
func cliVersion() {
	cfg := defaultConfig()

	state, err := cluster.Load(cfg.StatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Loading cluster state: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Running versions:")
	if state.Orchestrator != nil {
		v := state.Orchestrator.ImageVersion
		if v == "" {
			v = "(built from source)"
		}
		fmt.Printf("  orchestrator: %s\n", v)
	}
	for _, n := range state.Nodes {
		v := n.ImageVersion
		if v == "" {
			v = "(built from source)"
		}
		fmt.Printf("  %s: %s\n", n.ID, v)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fmt.Println("\nLatest available:")
	for _, vmType := range []string{"node", "orchestrator", "golden"} {
		meta, err := oci.Resolve(ctx, oci.PullOptions{
			Registry:   cfg.OCIRegistry,
			Repository: cfg.OCIRepository,
			VMType:     vmType,
			Tag:        "latest",
		})
		if err != nil {
			fmt.Printf("  %s: (not available)\n", vmType)
		} else {
			v := meta.Version
			if v == "" {
				v = meta.Digest[:12]
			}
			fmt.Printf("  %s: %s\n", vmType, v)
		}
	}
}

// cliBuildImage builds a VM image and optionally pushes to OCI registry.
//
//	boxcutter-host build-image <node|orchestrator|golden> [--push] [--tag TAG]
func cliBuildImage() {
	cfg := defaultConfig()

	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: boxcutter-host build-image <node|orchestrator|golden> [--push] [--tag TAG]\n")
		os.Exit(1)
	}

	vmType := os.Args[2]
	if vmType != "node" && vmType != "orchestrator" && vmType != "golden" {
		fmt.Fprintf(os.Stderr, "VM type must be 'node', 'orchestrator', or 'golden'\n")
		os.Exit(1)
	}

	push := false
	tag := ""
	for i, arg := range os.Args {
		if arg == "--push" {
			push = true
		}
		if arg == "--tag" && i+1 < len(os.Args) {
			tag = os.Args[i+1]
		}
	}

	// Get version from git
	version := ""
	commit := ""
	if out, err := exec.Command("git", "-C", cfg.RepoDir, "describe", "--tags", "--always").Output(); err == nil {
		version = string(out[:len(out)-1]) // trim newline
	}
	if out, err := exec.Command("git", "-C", cfg.RepoDir, "rev-parse", "--short", "HEAD").Output(); err == nil {
		commit = string(out[:len(out)-1])
	}
	if tag == "" {
		tag = version
	}
	if tag == "" {
		tag = "latest"
	}

	// Run the build script
	buildScript := filepath.Join(cfg.RepoDir, "host", "build-image.sh")
	if !fileExists(buildScript) {
		fmt.Fprintf(os.Stderr, "Build script not found: %s\n", buildScript)
		os.Exit(1)
	}

	fmt.Printf("Building %s image...\n", vmType)
	cmd := exec.Command("bash", buildScript, vmType)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = cfg.RepoDir
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Build failed: %v\n", err)
		os.Exit(1)
	}

	if !push {
		fmt.Printf("\nBuild complete. To push: boxcutter-host build-image %s --push --tag %s\n", vmType, tag)
		return
	}

	// Find the built image
	var ext string
	if vmType == "golden" {
		ext = "ext4"
	} else {
		ext = "qcow2"
	}
	imagePath := filepath.Join(cfg.ImagesDir, fmt.Sprintf("%s-image.%s.zst", vmType, ext))
	if !fileExists(imagePath) {
		fmt.Fprintf(os.Stderr, "Built image not found at %s\n", imagePath)
		os.Exit(1)
	}

	fmt.Printf("Pushing %s image (tag: %s)...\n", vmType, tag)
	ctx := context.Background()
	tags := []string{tag}
	if tag != "latest" {
		tags = append(tags, "latest")
	}

	if err := oci.Push(ctx, oci.PushOptions{
		Registry:   cfg.OCIRegistry,
		Repository: cfg.OCIRepository,
		VMType:     vmType,
		Tags:       tags,
		FilePath:   imagePath,
		Version:    version,
		Commit:     commit,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Push failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Pushed %s image with tags: %v\n", vmType, tags)
}

func parsePullArgs() (string, string) {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: boxcutter-host %s <node|orchestrator|golden|all> [--tag TAG]\n", os.Args[1])
		os.Exit(1)
	}

	vmType := os.Args[2]
	valid := map[string]bool{"node": true, "orchestrator": true, "golden": true, "all": true}
	if !valid[vmType] {
		fmt.Fprintf(os.Stderr, "VM type must be 'node', 'orchestrator', 'golden', or 'all'\n")
		os.Exit(1)
	}
	if vmType == "all" && os.Args[1] == "pull" {
		fmt.Fprintf(os.Stderr, "Cannot pull 'all' — specify 'node', 'orchestrator', or 'golden'\n")
		os.Exit(1)
	}

	tag := "latest"
	for i, arg := range os.Args {
		if arg == "--tag" && i+1 < len(os.Args) {
			tag = os.Args[i+1]
		}
	}

	return vmType, tag
}

func createCOWDisk(basePath, diskPath, size string) error {
	return runShell(fmt.Sprintf("qemu-img create -f qcow2 -b %s -F qcow2 %s %s",
		basePath, diskPath, size))
}

func decompressZstd(src, dst string) error {
	return runShell(fmt.Sprintf("zstd -d -f %s -o %s", src, dst))
}

func runShell(cmd string) error {
	c := exec.Command("sh", "-c", cmd)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func waitForNodeHealth(bridgeIP string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if health := queryNodeHealth(bridgeIP); health != nil {
			return true
		}
		time.Sleep(5 * time.Second)
	}
	return false
}

func runBootstrap() {
	cfg := defaultConfig()
	log.Println("boxcutter-host bootstrap")

	// Set up bridge
	if err := bridge.Setup(bridge.Config{
		BridgeDevice: cfg.BridgeDevice,
		BridgeIP:     cfg.BridgeIP,
		BridgeCIDR:   cfg.BridgeCIDR,
		HostNIC:      cfg.HostNIC,
	}); err != nil {
		log.Fatalf("Bridge setup: %v", err)
	}

	state, _ := cluster.Load(cfg.StatePath)

	currentUser, _ := user.Current()
	username := "root"
	if currentUser != nil {
		username = currentUser.Username
	}

	// Register orchestrator in state
	orchDisk := fmt.Sprintf("%s/orchestrator.qcow2", cfg.ImagesDir)
	orchISO := fmt.Sprintf("%s/orchestrator-cloud-init.iso", cfg.ImagesDir)

	if _, err := os.Stat(orchDisk); err != nil {
		log.Printf("Orchestrator disk not found at %s", orchDisk)
		log.Printf("Run: make provision-orchestrator")
		os.Exit(1)
	}

	bridge.EnsureTAP(cfg.OrchestratorTAP, cfg.BridgeDevice, username)

	orchPID, err := qemu.Launch(qemu.VMConfig{
		Name: "orchestrator",
		VCPU: cfg.OrchestratorVCPU,
		RAM:  cfg.OrchestratorRAM,
		Disk: orchDisk,
		ISO:  orchISO,
		TAP:  cfg.OrchestratorTAP,
		MAC:  cfg.OrchestratorMAC,
	}, cfg.ImagesDir)
	if err != nil {
		log.Fatalf("Orchestrator launch: %v", err)
	}

	state.SetOrchestrator(cluster.VMEntry{
		ID:       "orchestrator",
		BridgeIP: cfg.OrchestratorIP,
		Disk:     orchDisk,
		ISO:      orchISO,
		PID:      orchPID,
		VCPU:     cfg.OrchestratorVCPU,
		RAM:      cfg.OrchestratorRAM,
		TAP:      cfg.OrchestratorTAP,
		MAC:      cfg.OrchestratorMAC,
	})

	// Launch node-1
	node1Disk := fmt.Sprintf("%s/boxcutter-node-1.qcow2", cfg.ImagesDir)
	node1ISO := fmt.Sprintf("%s/boxcutter-node-1-cloud-init.iso", cfg.ImagesDir)

	if _, err := os.Stat(node1Disk); err == nil {
		bridge.EnsureTAP("tap-node1", cfg.BridgeDevice, username)
		node1PID, err := qemu.Launch(qemu.VMConfig{
			Name: "boxcutter-node-1",
			VCPU: cfg.NodeVCPU,
			RAM:  cfg.NodeRAM,
			Disk: node1Disk,
			ISO:  node1ISO,
			TAP:  "tap-node1",
			MAC:  "52:54:00:00:00:03",
		}, cfg.ImagesDir)
		if err != nil {
			log.Printf("WARNING: node-1 launch failed: %v", err)
		} else {
			state.AddNode(cluster.VMEntry{
				ID:       "boxcutter-node-1",
				BridgeIP: fmt.Sprintf("%s.3", cfg.NodeSubnet),
				Disk:     node1Disk,
				ISO:      node1ISO,
				PID:      node1PID,
				VCPU:     cfg.NodeVCPU,
				RAM:      cfg.NodeRAM,
				TAP:      "tap-node1",
				MAC:      "52:54:00:00:00:03",
			})
		}
	} else {
		log.Printf("No node-1 disk found — run: make provision-node")
	}

	if err := state.Save(); err != nil {
		log.Fatalf("Saving cluster state: %v", err)
	}

	log.Printf("Bootstrap complete. State saved to %s", cfg.StatePath)
}
