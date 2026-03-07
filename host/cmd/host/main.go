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
	"os/signal"
	"os/user"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AndrewBudd/boxcutter/host/internal/bridge"
	"github.com/AndrewBudd/boxcutter/host/internal/cluster"
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
	default:
		fmt.Fprintf(os.Stderr, "Usage: boxcutter-host <run|status|bootstrap>\n")
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
