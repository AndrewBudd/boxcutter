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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AndrewBudd/boxcutter/host/internal/bridge"
	"github.com/AndrewBudd/boxcutter/host/internal/cluster"
	"github.com/AndrewBudd/boxcutter/host/internal/oci"
	"github.com/AndrewBudd/boxcutter/host/internal/qemu"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

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
	ScalePollInterval     time.Duration
	HealthPollInterval    time.Duration
	ScaleUpThresholdPct   int           // Scale up when VM capacity used > this %
	ScaleDownThresholdPct int           // Scale down when VM capacity used < this % (and >1 node)
	ScaleCooldown         time.Duration // Minimum time between scale events
	MinFreeMemoryMB       int           // Hard floor: never scale up if host has less than this free
	DiskUsageThresholdPct int           // Scale up when node disk usage > this %
	MinFreeDiskMB         int           // Hard floor: never scale up if host has less than this free disk
	MaxNodes              int           // Hard cap on node count (0 = limited only by resources)

	// OCI image distribution
	OCIRegistry   string // OCI registry (default: ghcr.io)
	OCIRepository string // Repository path (default: AndrewBudd/boxcutter)

	// GitHub App auth for ghcr.io
	GitHubAppID          int64
	GitHubInstallationID int64
	GitHubPrivateKeyPath string
}

// detectDefaultNIC finds the network interface used for the default route.
// Falls back to "eth0" if detection fails.
func detectDefaultNIC() string {
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err == nil {
		// "default via 10.0.0.1 dev eth0 proto ..."
		fields := strings.Fields(string(out))
		for i, f := range fields {
			if f == "dev" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}
	return "eth0"
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
		// No repo found — use /var/lib/boxcutter as data directory (prod mode)
		repoDir = "/var/lib/boxcutter"
	}
	return HostConfig{
		RepoDir:            repoDir,
		ImagesDir:          repoDir + "/.images",
		HostNIC:            detectDefaultNIC(),
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
		ScalePollInterval:     30 * time.Second,
		HealthPollInterval:   10 * time.Second,
		ScaleUpThresholdPct:  80,
		ScaleDownThresholdPct: 30,
		ScaleCooldown:        10 * time.Minute,
		MinFreeMemoryMB:       8192,  // 8GB — never launch a node if host has less than this free
		DiskUsageThresholdPct: 85,    // Scale up when any node's disk > 85% full
		MinFreeDiskMB:         20480, // 20GB — never launch a node if host has less than this free disk
		MaxNodes:              0,     // 0 = no hard cap, limited only by host resources
		OCIRegistry:         oci.DefaultRegistry,
		OCIRepository:       oci.DefaultRepository,
		GitHubAppID:          3020803,
		GitHubInstallationID: 114361932,
		GitHubPrivateKeyPath: findGitHubAppKey(repoDir),
	}
}

func findGitHubAppKey(repoDir string) string {
	// Check repo-relative path first, then user home paths
	candidates := []string{
		filepath.Join(repoDir, ".boxcutter", "secrets", "github-app.pem"),
	}
	// Check SUDO_USER's home if running under sudo
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil {
			candidates = append(candidates, filepath.Join(u.HomeDir, ".boxcutter", "secrets", "github-app.pem"))
		}
	}
	// Current user's home
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".boxcutter", "secrets", "github-app.pem"))
	}
	for _, p := range candidates {
		if fileExists(p) {
			return p
		}
	}
	return candidates[0] // return default even if not found
}

func (cfg HostConfig) ociAuth() *oci.GitHubAppAuth {
	if !fileExists(cfg.GitHubPrivateKeyPath) {
		return nil
	}
	return &oci.GitHubAppAuth{
		AppID:          cfg.GitHubAppID,
		InstallationID: cfg.GitHubInstallationID,
		PrivateKeyPath: cfg.GitHubPrivateKeyPath,
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
	case "push-golden":
		cliPushGolden()
	case "recover":
		cliRecover()
	case "self-update":
		cliSelfUpdate()
	default:
		fmt.Fprintf(os.Stderr, "Usage: boxcutter-host <run|status|bootstrap|pull|upgrade|recover|self-update|version|build-image|push-golden>\n\n")
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  run                          Run the host daemon\n")
		fmt.Fprintf(os.Stderr, "  status                       Show cluster status\n")
		fmt.Fprintf(os.Stderr, "  bootstrap                    Pull OCI images and create VMs (prod)\n")
		fmt.Fprintf(os.Stderr, "  bootstrap --from-source      Build from local repo and create VMs (dev)\n")
		fmt.Fprintf(os.Stderr, "  bootstrap --version v0.1.0   Pull specific version of OCI images\n")
		fmt.Fprintf(os.Stderr, "  recover                      Scan for running VMs and rebuild cluster.json\n")
		fmt.Fprintf(os.Stderr, "  self-update [--version TAG]  Update boxcutter-host to latest stable release\n")
		fmt.Fprintf(os.Stderr, "  pull <type> [--tag TAG]      Pull a VM image from OCI registry\n")
		fmt.Fprintf(os.Stderr, "  upgrade <type> [--tag TAG]   Rolling upgrade of VMs\n")
		os.Exit(1)
	}
}

func runDaemon() {
	cfg := defaultConfig()

	log.Println("boxcutter-host starting...")

	// 0. Start mosquitto broker
	mosquittoProc := startMosquitto(cfg)
	if mosquittoProc != nil {
		defer func() {
			log.Println("Stopping mosquitto...")
			mosquittoProc.Process.Signal(syscall.SIGTERM)
			mosquittoProc.Wait()
		}()
	}

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

	// 4. Initialize health tracking
	hs := newHealthState()

	// 5. Start unix socket API
	go startAPI(cfg, state, hs)

	// 6. Start health monitor
	go healthLoop(cfg, state, hs)

	// 7. Start auto-scaler
	go autoScaleLoop(cfg, state)

	// 8. Resume any interrupted upgrade in background
	if state.UpgradeGoal != nil {
		log.Printf("Found incomplete upgrade goal: %s (tag: %s)", state.UpgradeGoal.VMType, state.UpgradeGoal.Tag)
		go runReconcileLoop(cfg, state)
	}

	log.Println("boxcutter-host ready")

	// Wait for shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("boxcutter-host shutting down")
}

// discoverOrphanedVMs scans /proc for running qemu-system-x86_64 processes
// that are not tracked in the cluster state. This handles the case where
// cluster.json was lost or corrupted but VMs are still running.
func discoverOrphanedVMs(cfg HostConfig, state *cluster.State) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		log.Printf("WARNING: cannot scan /proc: %v", err)
		return
	}

	// Build set of PIDs already tracked
	knownPIDs := map[int]bool{}
	if state.Orchestrator != nil {
		knownPIDs[state.Orchestrator.PID] = true
	}
	for _, n := range state.Nodes {
		knownPIDs[n.PID] = true
	}

	discovered := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if knownPIDs[pid] {
			continue
		}

		// Read cmdline
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		if len(args) < 2 || !strings.HasSuffix(args[0], "qemu-system-x86_64") {
			continue
		}

		// Parse QEMU args to extract VM identity
		vmEntry := parseQEMUArgs(args, pid)
		if vmEntry == nil {
			continue
		}

		if vmEntry.Type == "orchestrator" {
			if state.Orchestrator != nil && qemu.IsRunning(state.Orchestrator.PID) {
				continue // already have a running orchestrator
			}
			log.Printf("  Discovered orphaned orchestrator (PID %d, disk %s)", pid, vmEntry.Disk)
			state.SetOrchestrator(*vmEntry)
			discovered++
		} else if vmEntry.Type == "node" {
			existing := state.GetNode(vmEntry.ID)
			if existing != nil && qemu.IsRunning(existing.PID) {
				continue
			}
			log.Printf("  Discovered orphaned node %s (PID %d, disk %s)", vmEntry.ID, pid, vmEntry.Disk)
			state.AddNode(*vmEntry)
			discovered++
		}
	}

	if discovered > 0 {
		log.Printf("  Recovered %d orphaned VM(s) from running QEMU processes", discovered)
		state.Save()
	}
}

// parseQEMUArgs extracts VM identity from QEMU command-line arguments.
func parseQEMUArgs(args []string, pid int) *cluster.VMEntry {
	entry := &cluster.VMEntry{PID: pid}

	nodeNameRe := regexp.MustCompile(`boxcutter-node-(\d+)`)

	for i, arg := range args {
		switch {
		case strings.HasPrefix(arg, "file=") && strings.Contains(arg, ".qcow2"):
			// "-drive" "file=/path/to/vm.qcow2,format=qcow2,if=virtio"
			parts := strings.SplitN(arg, ",", 2)
			entry.Disk = strings.TrimPrefix(parts[0], "file=")
		case i > 0 && args[i-1] == "-drive" && strings.HasPrefix(arg, "file=") && strings.Contains(arg, ".qcow2"):
			parts := strings.SplitN(arg, ",", 2)
			entry.Disk = strings.TrimPrefix(parts[0], "file=")
		case strings.HasPrefix(arg, "tap,id="):
			// "-netdev" "tap,id=net0,ifname=tap-node1,script=no,downscript=no"
			for _, kv := range strings.Split(arg, ",") {
				if strings.HasPrefix(kv, "ifname=") {
					entry.TAP = strings.TrimPrefix(kv, "ifname=")
				}
			}
		case strings.HasPrefix(arg, "virtio-net-pci,netdev="):
			// "-device" "virtio-net-pci,netdev=net0,mac=52:54:00:00:00:03"
			for _, kv := range strings.Split(arg, ",") {
				if strings.HasPrefix(kv, "mac=") {
					entry.MAC = strings.TrimPrefix(kv, "mac=")
				}
			}
		case arg == "-smp" && i+1 < len(args):
			entry.VCPU, _ = strconv.Atoi(args[i+1])
		case arg == "-m" && i+1 < len(args):
			entry.RAM = args[i+1]
		}
	}

	if entry.Disk == "" {
		return nil
	}

	// Identify VM type and name from disk path or TAP name
	diskBase := filepath.Base(entry.Disk)
	if strings.HasPrefix(diskBase, "orchestrator") {
		entry.Type = "orchestrator"
		entry.ID = "orchestrator"
		entry.BridgeIP = "192.168.50.2" // convention
	} else if m := nodeNameRe.FindStringSubmatch(diskBase); m != nil {
		entry.Type = "node"
		entry.ID = "boxcutter-node-" + m[1]
		nodeNum, _ := strconv.Atoi(m[1])
		entry.BridgeIP = fmt.Sprintf("192.168.50.%d", 2+nodeNum)
	} else if m := nodeNameRe.FindStringSubmatch(entry.TAP); m != nil {
		entry.Type = "node"
		entry.ID = "boxcutter-node-" + m[1]
		nodeNum, _ := strconv.Atoi(m[1])
		entry.BridgeIP = fmt.Sprintf("192.168.50.%d", 2+nodeNum)
	} else {
		return nil // unrecognized VM
	}

	// Find the cloud-init ISO from args
	for _, arg := range args {
		if strings.HasPrefix(arg, "file=") && strings.Contains(arg, "cloud-init.iso") {
			parts := strings.SplitN(arg, ",", 2)
			entry.ISO = strings.TrimPrefix(parts[0], "file=")
		}
	}

	return entry
}

func bootRecover(cfg HostConfig, state *cluster.State) {
	// First, discover any running QEMU VMs not tracked in state
	discoverOrphanedVMs(cfg, state)

	currentUser, _ := user.Current()
	username := "root"
	if currentUser != nil {
		username = currentUser.Username
	}

	// Launch orchestrator (skip if being upgraded)
	if state.Orchestrator != nil {
		orch := state.Orchestrator
		if !orch.IsActive() {
			log.Printf("  orchestrator status=%s, skipping relaunch", orch.Status)
		} else if qemu.IsRunning(orch.PID) {
			log.Printf("  orchestrator already running (PID %d)", orch.PID)
		} else {
			if !fileExists(orch.Disk) {
				log.Printf("WARNING: orchestrator disk missing: %s", orch.Disk)
			} else {
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
		}
	}

	// Launch nodes (skip those being drained/upgraded)
	for _, node := range state.Nodes {
		if !node.IsActive() {
			log.Printf("  %s status=%s, skipping relaunch", node.ID, node.Status)
			continue
		}
		if qemu.IsRunning(node.PID) {
			log.Printf("  %s already running (PID %d)", node.ID, node.PID)
			continue
		}
		if !fileExists(node.Disk) {
			log.Printf("WARNING: %s disk missing: %s", node.ID, node.Disk)
			continue
		}
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

// serviceHealth tracks application-level health for VMs.
type serviceHealth struct {
	healthy          bool
	consecutiveFails int
	lastCheck        time.Time
	lastHealthy      time.Time
}

// healthState tracks service health across health loop iterations.
// Protected by mu since healthLoop writes and API handlers read concurrently.
type healthState struct {
	mu           sync.RWMutex
	startTime    time.Time
	orchestrator serviceHealth
	nodes        map[string]*serviceHealth // keyed by node ID
}

func newHealthState() *healthState {
	return &healthState{
		startTime: time.Now(),
		nodes:     make(map[string]*serviceHealth),
	}
}

const serviceUnhealthyThreshold = 3 // consecutive failed checks before marking unhealthy

// healthLoop periodically checks that all VMs are running and services are responsive.
func healthLoop(cfg HostConfig, state *cluster.State, hs *healthState) {
	ticker := time.NewTicker(cfg.HealthPollInterval)
	defer ticker.Stop()

	client := &http.Client{Timeout: 2 * time.Second}

	for range ticker.C {
		hs.mu.Lock()

		// Check orchestrator (skip if draining/upgrading)
		if state.Orchestrator != nil && state.Orchestrator.IsActive() {
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
				hs.orchestrator.healthy = false
			} else {
				// QEMU is running — check application-level health
				checkServiceHealth(client, "orchestrator",
					fmt.Sprintf("http://%s:8801/healthz", state.Orchestrator.BridgeIP),
					&hs.orchestrator)
			}
		}

		// Check nodes (skip those being drained/upgraded)
		for _, node := range state.Nodes {
			if !node.IsActive() {
				continue
			}
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
				if hs.nodes[node.ID] == nil {
					hs.nodes[node.ID] = &serviceHealth{}
				}
				hs.nodes[node.ID].healthy = false
			} else {
				// QEMU is running — check application-level health
				if hs.nodes[node.ID] == nil {
					hs.nodes[node.ID] = &serviceHealth{}
				}
				checkServiceHealth(client, node.ID,
					fmt.Sprintf("http://%s:8800/api/health", node.BridgeIP),
					hs.nodes[node.ID])
			}
		}

		// Clean up stale node entries from healthState
		activeNodes := make(map[string]bool)
		for _, node := range state.Nodes {
			activeNodes[node.ID] = true
		}
		for id := range hs.nodes {
			if !activeNodes[id] {
				delete(hs.nodes, id)
			}
		}

		hs.mu.Unlock()
	}
}

// checkServiceHealth hits a health endpoint and updates the serviceHealth tracker.
func checkServiceHealth(client *http.Client, name, url string, sh *serviceHealth) {
	sh.lastCheck = time.Now()
	resp, err := client.Get(url)
	if err != nil {
		sh.consecutiveFails++
		if sh.consecutiveFails == serviceUnhealthyThreshold {
			log.Printf("WARNING: %s service unreachable for %d consecutive checks", name, sh.consecutiveFails)
			sh.healthy = false
		}
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if !sh.healthy && sh.consecutiveFails >= serviceUnhealthyThreshold {
			log.Printf("INFO: %s service recovered after %d failed checks", name, sh.consecutiveFails)
		}
		sh.healthy = true
		sh.consecutiveFails = 0
		sh.lastHealthy = time.Now()
	} else {
		sh.consecutiveFails++
		if sh.consecutiveFails == serviceUnhealthyThreshold {
			log.Printf("WARNING: %s service returning %d for %d consecutive checks", name, resp.StatusCode, sh.consecutiveFails)
			sh.healthy = false
		}
	}
}

// nodeCapacity holds per-node capacity info collected during a poll.
type nodeCapacity struct {
	nodeID       string
	bridgeIP     string
	totalRAM     int
	usedRAM      int
	totalVCPU    int
	usedVCPU     int
	diskTotalMB  int
	diskUsedMB   int
	vmsRunning   int
}

// scaleDownCandidate evaluates whether to scale down and returns the node ID to drain.
// Returns empty string if no scale-down should happen.
func scaleDownCandidate(nodes []nodeCapacity, totalRAM, usedRAM, scaleDownPct, scaleUpPct int) string {
	if len(nodes) <= 1 || totalRAM == 0 {
		return ""
	}

	usedPct := (usedRAM * 100) / totalRAM
	if usedPct > scaleDownPct {
		return ""
	}

	// Find the least-loaded node
	leastIdx := 0
	for i, nc := range nodes {
		if nc.usedRAM < nodes[leastIdx].usedRAM {
			leastIdx = i
		}
	}
	candidate := nodes[leastIdx]

	// Check: would removing this node push remaining capacity above scale-up threshold?
	remainRAM := totalRAM - candidate.totalRAM
	if remainRAM <= 0 {
		return ""
	}
	// After drain, all VMs migrate to remaining nodes, so total used RAM stays the same
	afterPct := (usedRAM * 100) / remainRAM
	if afterPct >= scaleUpPct {
		return ""
	}

	return candidate.nodeID
}

// autoScaleLoop polls nodes for capacity and scales up/down.
func autoScaleLoop(cfg HostConfig, state *cluster.State) {
	// Wait for VMs to boot before polling
	time.Sleep(30 * time.Second)

	ticker := time.NewTicker(cfg.ScalePollInterval)
	defer ticker.Stop()

	var lastScaleEvent time.Time

	for range ticker.C {
		if state.NodeCount() == 0 {
			continue
		}

		var nodes []nodeCapacity
		totalRAM := 0
		usedRAM := 0
		totalVCPU := 0
		usedVCPU := 0
		totalDiskMB := 0
		usedDiskMB := 0
		totalVMs := 0

		for _, node := range state.Nodes {
			if !node.IsActive() || !qemu.IsRunning(node.PID) {
				continue
			}
			health := queryNodeHealth(node.BridgeIP)
			if health == nil {
				continue
			}
			nc := nodeCapacity{nodeID: node.ID, bridgeIP: node.BridgeIP}
			if v, ok := health["ram_total_mib"].(float64); ok {
				nc.totalRAM = int(v)
				totalRAM += int(v)
			}
			if v, ok := health["ram_allocated_mib"].(float64); ok {
				nc.usedRAM = int(v)
				usedRAM += int(v)
			}
			if v, ok := health["vcpu_total"].(float64); ok {
				nc.totalVCPU = int(v)
				totalVCPU += int(v)
			}
			if v, ok := health["vcpu_allocated"].(float64); ok {
				nc.usedVCPU = int(v)
				usedVCPU += int(v)
			}
			if v, ok := health["disk_total_mb"].(float64); ok {
				nc.diskTotalMB = int(v)
				totalDiskMB += int(v)
			}
			if v, ok := health["disk_used_mb"].(float64); ok {
				nc.diskUsedMB = int(v)
				usedDiskMB += int(v)
			}
			if v, ok := health["vms_running"].(float64); ok {
				nc.vmsRunning = int(v)
				totalVMs += int(v)
			}
			nodes = append(nodes, nc)
		}

		if totalRAM == 0 {
			continue
		}

		usedPct := (usedRAM * 100) / totalRAM
		cpuPct := 0
		if totalVCPU > 0 {
			cpuPct = (usedVCPU * 100) / totalVCPU
		}
		diskPct := 0
		if totalDiskMB > 0 {
			diskPct = (usedDiskMB * 100) / totalDiskMB
		}
		log.Printf("Capacity: RAM %d/%d MiB (%d%%), CPU %d/%d vCPU (%d%%), Disk %d/%dMB (%d%%), %d VMs across %d nodes",
			usedRAM, totalRAM, usedPct, usedVCPU, totalVCPU, cpuPct, usedDiskMB, totalDiskMB, diskPct, totalVMs, len(nodes))

		// Enforce cooldown between scale events
		if time.Since(lastScaleEvent) < cfg.ScaleCooldown {
			continue
		}

		// Scale up if RAM, CPU, or disk usage exceeds threshold
		scaleUpReason := ""
		if usedPct >= cfg.ScaleUpThresholdPct {
			scaleUpReason = fmt.Sprintf("RAM at %d%%", usedPct)
		} else if cpuPct >= cfg.ScaleUpThresholdPct {
			scaleUpReason = fmt.Sprintf("CPU at %d%%", cpuPct)
		} else if diskPct >= cfg.DiskUsageThresholdPct {
			scaleUpReason = fmt.Sprintf("disk at %d%%", diskPct)
		}
		if scaleUpReason != "" {
			log.Printf("Capacity pressure (%s), checking if scale-up is possible...", scaleUpReason)
			ok, reason := canScaleUp(cfg, state.NodeCount())
			if ok {
				log.Printf("Scaling up: adding new node (%s)...", scaleUpReason)
				addNode(cfg, state)
				lastScaleEvent = time.Now()
			} else {
				log.Printf("Cannot scale up: %s", reason)
			}
		} else if candidateID := scaleDownCandidate(nodes, totalRAM, usedRAM, cfg.ScaleDownThresholdPct, cfg.ScaleUpThresholdPct); candidateID != "" {
			// Find the candidate's info for logging
			for _, nc := range nodes {
				if nc.nodeID == candidateID {
					log.Printf("Capacity below %d%% with %d nodes, scaling down: draining %s (%d MiB used, %d VMs)",
						cfg.ScaleDownThresholdPct, len(nodes), nc.nodeID, nc.usedRAM, nc.vmsRunning)
					break
				}
			}
			drainNode(cfg, state, candidateID)
			lastScaleEvent = time.Now()
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

func canScaleUp(cfg HostConfig, currentNodeCount int) (bool, string) {
	// Hard cap: configured MaxNodes (0 = unlimited)
	if cfg.MaxNodes > 0 && currentNodeCount >= cfg.MaxNodes {
		return false, fmt.Sprintf("at configured maximum of %d nodes", cfg.MaxNodes)
	}

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

	// Parse node RAM requirement (e.g. "12G")
	var nodeRAMMB int
	fmt.Sscanf(cfg.NodeRAM, "%dG", &nodeRAMMB)
	nodeRAMMB *= 1024
	if nodeRAMMB == 0 {
		nodeRAMMB = 12 * 1024 // fallback
	}

	// Rolling upgrade reserve: after launching this node, there must still be
	// enough memory to launch ONE MORE node (for rolling upgrades). This ensures
	// we can always do a rolling upgrade without first draining a node.
	// Required: availMB - thisNode - upgradeReserve >= MinFreeMemoryMB
	requiredMB := nodeRAMMB*2 + cfg.MinFreeMemoryMB
	if availMB < requiredMB {
		afterLaunchMB := availMB - nodeRAMMB
		return false, fmt.Sprintf("after launch would have %dMB free (%dMB available - %dMB node), "+
			"need %dMB reserved for rolling upgrade + %dMB minimum free",
			afterLaunchMB, availMB, nodeRAMMB, nodeRAMMB, cfg.MinFreeMemoryMB)
	}

	// Also enforce MaxNodes against memory-based ceiling:
	// memory-based max = floor((availMB - MinFreeMemoryMB) / nodeRAMMB) - 1 (rolling upgrade reserve)
	memoryBasedMax := (availMB - cfg.MinFreeMemoryMB) / nodeRAMMB
	if memoryBasedMax > 1 {
		memoryBasedMax-- // reserve one slot for rolling upgrade
	}
	effectiveMax := memoryBasedMax
	if cfg.MaxNodes > 0 && cfg.MaxNodes < effectiveMax {
		effectiveMax = cfg.MaxNodes
	}
	if currentNodeCount >= effectiveMax {
		return false, fmt.Sprintf("at capacity: %d nodes running, effective max is %d (memory-based: %d, configured: %d, includes rolling upgrade reserve)",
			currentNodeCount, effectiveMax, memoryBasedMax, cfg.MaxNodes)
	}

	// Check host disk space
	var diskAvailMB int
	dfOut, err := exec.Command("df", "-BM", "--output=avail", cfg.ImagesDir).Output()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(dfOut)), "\n")
		if len(lines) >= 2 {
			fmt.Sscanf(strings.ReplaceAll(lines[1], "M", ""), "%d", &diskAvailMB)
		}
	}
	if diskAvailMB > 0 && diskAvailMB < cfg.MinFreeDiskMB {
		return false, fmt.Sprintf("host has %dMB disk free, minimum is %dMB", diskAvailMB, cfg.MinFreeDiskMB)
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

	// Auto-provision disk from base image if not already present
	if _, err := os.Stat(disk); err != nil {
		log.Printf("Node %s disk not found, provisioning from base image...", nodeID)
		provisionCmd := exec.Command("bash", hostFile(cfg, "provision.sh"),
			"node", nodeID, "--from-image")
		provisionCmd.Dir = cfg.RepoDir
		provisionCmd.Env = append(os.Environ(), "BOXCUTTER_IMAGES_DIR="+cfg.ImagesDir)
		if out, err := provisionCmd.CombinedOutput(); err != nil {
			log.Printf("Failed to provision %s: %v\n%s", nodeID, err, string(out))
			return
		}
		log.Printf("Node %s provisioned successfully", nodeID)
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
func startAPI(cfg HostConfig, state *cluster.State, hs *healthState) {
	os.Remove(cfg.SocketPath)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		hs.mu.RLock()
		nodesHealthy := 0
		nodesUnhealthy := 0
		for _, node := range state.Nodes {
			if sh := hs.nodes[node.ID]; sh != nil && sh.healthy {
				nodesHealthy++
			} else {
				nodesUnhealthy++
			}
		}
		orchHealthy := hs.orchestrator.healthy
		uptime := int(time.Since(hs.startTime).Seconds())
		hs.mu.RUnlock()

		status := "ok"
		httpStatus := http.StatusOK
		if !orchHealthy || nodesUnhealthy > 0 {
			status = "degraded"
			httpStatus = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":               status,
			"uptime_seconds":       uptime,
			"orchestrator_healthy": orchHealthy,
			"nodes_healthy":        nodesHealthy,
			"nodes_unhealthy":      nodesUnhealthy,
		})
	})

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		status := buildStatus(cfg, state, hs)
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

func buildStatus(cfg HostConfig, state *cluster.State, hs *healthState) map[string]interface{} {
	hs.mu.RLock()
	uptime := int(time.Since(hs.startTime).Seconds())

	result := map[string]interface{}{
		"uptime_seconds": uptime,
	}

	if state.Orchestrator != nil {
		orch := state.Orchestrator
		orchStatus := map[string]interface{}{
			"id":              orch.ID,
			"bridge_ip":       orch.BridgeIP,
			"pid":             orch.PID,
			"running":         qemu.IsRunning(orch.PID),
			"service_healthy": hs.orchestrator.healthy,
		}
		if !hs.orchestrator.lastHealthy.IsZero() {
			orchStatus["last_healthy"] = hs.orchestrator.lastHealthy.Format(time.RFC3339)
		}
		result["orchestrator"] = orchStatus
	}

	// Scaling capacity info
	nodeRAMMB := 0
	fmt.Sscanf(cfg.NodeRAM, "%dG", &nodeRAMMB)
	nodeRAMMB *= 1024
	if nodeRAMMB == 0 {
		nodeRAMMB = 12 * 1024
	}
	result["scaling"] = map[string]interface{}{
		"current_nodes": state.NodeCount(),
		"max_nodes":     cfg.MaxNodes,
		"node_ram_mb":   nodeRAMMB,
	}

	nodes := []map[string]interface{}{}
	for _, n := range state.Nodes {
		nodeStatus := map[string]interface{}{
			"id":        n.ID,
			"bridge_ip": n.BridgeIP,
			"pid":       n.PID,
			"running":   qemu.IsRunning(n.PID),
		}
		if sh := hs.nodes[n.ID]; sh != nil {
			nodeStatus["service_healthy"] = sh.healthy
			if !sh.lastHealthy.IsZero() {
				nodeStatus["last_healthy"] = sh.lastHealthy.Format(time.RFC3339)
			}
		}
		nodes = append(nodes, nodeStatus)
	}
	hs.mu.RUnlock()

	// Fetch live health from node agents (outside lock — these are network calls)
	for i, n := range state.Nodes {
		if qemu.IsRunning(n.PID) {
			if health := queryNodeHealth(n.BridgeIP); health != nil {
				nodes[i]["health"] = health
			}
		}
	}
	result["nodes"] = nodes

	if state.UpgradeGoal != nil {
		result["upgrade"] = state.UpgradeGoal
	}

	return result
}

// drainNode migrates all Firecrackers off a node, then stops it.
func drainNode(cfg HostConfig, state *cluster.State, nodeID string) {
	node := state.GetNode(nodeID)
	if node == nil {
		log.Printf("Drain: node %s not found", nodeID)
		return
	}

	// Mark as draining so health monitor won't auto-restart if we stop it
	state.SetNodeStatus(nodeID, "draining")
	state.Save()

	log.Printf("Draining node %s...", nodeID)

	// Get list of VMs on this node
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:8800/api/vms", node.BridgeIP))
	if err != nil {
		log.Printf("Drain: cannot list VMs on %s: %v", nodeID, err)
		state.SetNodeStatus(nodeID, "active") // revert
		state.Save()
		return
	}
	defer resp.Body.Close()

	var vms []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&vms); err != nil {
		log.Printf("Drain: invalid VM list from %s: %v", nodeID, err)
		state.SetNodeStatus(nodeID, "active") // revert
		state.Save()
		return
	}

	if len(vms) == 0 {
		log.Printf("Drain: %s has no VMs, stopping", nodeID)
		qemu.Stop(nodeID, node.PID)
		state.RemoveNode(nodeID)
		state.Save()
		return
	}

	// Find a target node (must be active and running)
	var targetNode *cluster.VMEntry
	for _, n := range state.Nodes {
		if n.ID != nodeID && n.IsActive() && qemu.IsRunning(n.PID) {
			entry := n
			targetNode = &entry
			break
		}
	}
	if targetNode == nil {
		log.Printf("Drain: no active target node available for migration")
		state.SetNodeStatus(nodeID, "active") // revert
		state.Save()
		return
	}

	// Migrate VMs one at a time — each migration is async on the node,
	// we poll until it completes before starting the next one
	migrateClient := &http.Client{Timeout: 30 * time.Second}
	pollClient := &http.Client{Timeout: 5 * time.Second}
	failedMigrations := 0

	for _, vm := range vms {
		vmName, ok := vm["name"].(string)
		if !ok {
			continue
		}

		vmStatus, _ := vm["status"].(string)
		log.Printf("Drain: migrating %s from %s to %s (status: %s)", vmName, nodeID, targetNode.ID, vmStatus)

		migrateReq := map[string]string{
			"target_addr":      fmt.Sprintf("%s:8800", targetNode.BridgeIP),
			"target_bridge_ip": targetNode.BridgeIP,
		}
		data, _ := json.Marshal(migrateReq)

		migrateResp, err := migrateClient.Post(
			fmt.Sprintf("http://%s:8800/api/vms/%s/migrate", node.BridgeIP, vmName),
			"application/json",
			jsonReader(data),
		)
		if err != nil {
			log.Printf("Drain: failed to start migration of %s: %v", vmName, err)
			failedMigrations++
			continue
		}
		migrateResp.Body.Close()
		if migrateResp.StatusCode >= 300 {
			log.Printf("Drain: migrate %s returned %d", vmName, migrateResp.StatusCode)
			failedMigrations++
			continue
		}

		// Poll source status until migration completes (up to 5 min per VM).
		deadline := time.Now().Add(5 * time.Minute)
		migrated := false
		for time.Now().Before(deadline) {
			time.Sleep(5 * time.Second)

			srcResp, err := pollClient.Get(fmt.Sprintf("http://%s:8800/api/vms/%s", node.BridgeIP, vmName))
			if err != nil {
				// Source unreachable — need to verify at target
				log.Printf("Drain: %s source unreachable, verifying at target...", vmName)
				migrated = true
				break
			}
			var srcDetail map[string]interface{}
			json.NewDecoder(srcResp.Body).Decode(&srcDetail)
			srcResp.Body.Close()

			srcStatus, _ := srcDetail["status"].(string)

			if srcStatus == "migrating" {
				continue // still in progress
			}

			if srcStatus == "" {
				// VM gone from source
				log.Printf("Drain: %s removed from source", vmName)
				migrated = true
				break
			}

			// VM is back to running/stopped — migration failed and rolled back
			log.Printf("Drain: %s migration failed (source status: %s)", vmName, srcStatus)
			break
		}

		// Improvement 3: verify migration at destination
		if migrated {
			verifyResp, err := pollClient.Get(
				fmt.Sprintf("http://%s:8800/api/vms/%s", targetNode.BridgeIP, vmName))
			if err != nil {
				log.Printf("Drain: WARNING: %s gone from source but target %s unreachable: %v",
					vmName, targetNode.ID, err)
				failedMigrations++
				migrated = false
			} else {
				var targetDetail map[string]interface{}
				json.NewDecoder(verifyResp.Body).Decode(&targetDetail)
				verifyResp.Body.Close()
				targetStatus, _ := targetDetail["status"].(string)
				if targetStatus == "" {
					log.Printf("Drain: WARNING: %s not found on target %s after migration",
						vmName, targetNode.ID)
					failedMigrations++
					migrated = false
				} else {
					log.Printf("Drain: %s verified on target %s (status: %s)",
						vmName, targetNode.ID, targetStatus)
				}
			}
		}

		if !migrated {
			log.Printf("Drain: %s migration did not complete", vmName)
			failedMigrations++
		}
	}

	if failedMigrations > 0 {
		log.Printf("Drain: %d migration(s) failed — aborting drain, node %s NOT stopped", failedMigrations, nodeID)
		state.SetNodeStatus(nodeID, "active") // revert so health monitor manages it
		state.Save()
		return
	}

	// All VMs migrated and verified — stop the node
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

// hostFile resolves a host script/config file path. In dev mode (BOXCUTTER_REPO set),
// files are at $REPO/host/FILE. In prod/deb mode, files are at /usr/share/boxcutter/FILE.
func hostFile(cfg HostConfig, name string) string {
	repoPath := filepath.Join(cfg.RepoDir, "host", name)
	if fileExists(repoPath) {
		return repoPath
	}
	debPath := filepath.Join("/usr/share/boxcutter", name)
	if fileExists(debPath) {
		return debPath
	}
	return repoPath // fallback to repo path for error messages
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
		Auth:       cfg.ociAuth(),
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

// cliUpgrade sets an upgrade goal and runs the reconciler to convergence.
//
//	boxcutter-host upgrade <node|orchestrator|all> [--tag TAG]
func cliUpgrade() {
	cfg := defaultConfig()
	vmType, tag := parsePullArgs()

	if vmType == "golden" {
		state, err := cluster.Load(cfg.StatePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Loading cluster state: %v\n", err)
			os.Exit(1)
		}
		upgradeGolden(cfg, state, tag)
		return
	}

	state, err := cluster.Load(cfg.StatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Loading cluster state: %v\n", err)
		os.Exit(1)
	}

	// Resume existing goal or set a new one
	if state.UpgradeGoal != nil {
		fmt.Printf("Resuming upgrade: %s (tag: %s)\n", state.UpgradeGoal.VMType, state.UpgradeGoal.Tag)
	} else {
		state.SetUpgradeGoal(&cluster.UpgradeGoal{
			VMType:           vmType,
			Tag:              tag,
			InitialNodeCount: state.ActiveNodeCount(),
		})
		state.Save()
		fmt.Printf("Upgrade goal set: %s (tag: %s)\n", vmType, tag)
	}

	// Run reconciler to convergence
	for {
		done, action, err := reconcileUpgradeStep(cfg, state)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Upgrade step failed: %v\n", err)
			fmt.Fprintf(os.Stderr, "Goal preserved in cluster.json — re-run 'boxcutter-host upgrade' to retry\n")
			os.Exit(1)
		}
		if action != "" {
			fmt.Printf("  %s\n", action)
		}
		if done {
			fmt.Println("Upgrade complete.")
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// runReconcileLoop runs the upgrade reconciler in a background goroutine
// until the goal is satisfied. Used by the daemon on startup to resume
// interrupted upgrades.
func runReconcileLoop(cfg HostConfig, state *cluster.State) {
	for {
		done, action, err := reconcileUpgradeStep(cfg, state)
		if err != nil {
			log.Printf("Upgrade reconciliation failed: %v (retrying in 30s)", err)
			time.Sleep(30 * time.Second)
			continue
		}
		if action != "" {
			log.Printf("[reconcile] %s", action)
		}
		if done {
			log.Println("Upgrade reconciliation complete")
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// reconcileUpgradeStep observes the cluster state against the UpgradeGoal
// and takes ONE step toward convergence. Returns done=true when the goal
// is fully satisfied. Each step is idempotent — crash and re-entry is safe.
func reconcileUpgradeStep(cfg HostConfig, state *cluster.State) (done bool, action string, err error) {
	goal := state.UpgradeGoal
	if goal == nil {
		return true, "", nil
	}

	needsNode := goal.VMType == "node" || goal.VMType == "all"
	needsOrch := goal.VMType == "orchestrator" || goal.VMType == "all"

	// --- Step 1: Ensure images are pulled ---

	if needsNode && goal.NodeImage == nil {
		ref, basePath, err := pullAndDecompress(cfg, "node", goal.Tag)
		if err != nil {
			return false, "", fmt.Errorf("pulling node image: %w", err)
		}
		goal.NodeImage = ref
		goal.NodeBasePath = basePath
		state.Save()
		return false, fmt.Sprintf("Pulled node image %s", ref.Version), nil
	}

	if needsOrch && goal.OrchImage == nil {
		ref, basePath, err := pullAndDecompress(cfg, "orchestrator", goal.Tag)
		if err != nil {
			return false, "", fmt.Errorf("pulling orchestrator image: %w", err)
		}
		goal.OrchImage = ref
		goal.OrchBasePath = basePath
		state.Save()
		return false, fmt.Sprintf("Pulled orchestrator image %s", ref.Version), nil
	}

	// --- Step 2: Node upgrades (rolling, one at a time) ---

	if needsNode {
		stepDone, stepAction, stepErr := reconcileNodeUpgrade(cfg, state, goal)
		if stepErr != nil || !stepDone {
			return false, stepAction, stepErr
		}
		// All nodes match goal — fall through to check orchestrator
	}

	// --- Step 3: Orchestrator upgrade ---

	if needsOrch && orchNeedsUpgrade(state, goal) {
		stepDone, stepAction, stepErr := reconcileOrchUpgrade(cfg, state, goal)
		if stepErr != nil || !stepDone {
			return false, stepAction, stepErr
		}
	}

	// --- All done ---
	state.ClearUpgradeGoal()
	state.Save()
	return true, "All VMs upgraded", nil
}

// reconcileNodeUpgrade handles one step of rolling node upgrades.
func reconcileNodeUpgrade(cfg HostConfig, state *cluster.State, goal *cluster.UpgradeGoal) (done bool, action string, err error) {
	// Is there a node currently being drained?
	if n := state.FindNodeWithStatus("draining"); n != nil {
		drainNode(cfg, state, n.ID)
		return false, fmt.Sprintf("Drained old node %s", n.ID), nil
	}

	// Is there a node marked as upgrading (needs to be drained)?
	if n := state.FindNodeWithStatus("upgrading"); n != nil {
		state.SetNodeStatus(n.ID, "draining")
		state.Save()
		drainNode(cfg, state, n.ID)
		return false, fmt.Sprintf("Drained upgrading node %s", n.ID), nil
	}

	// Find the first old node that doesn't match the goal
	oldNode := firstNodeNotMatchingGoal(state, goal)
	if oldNode == nil {
		return true, "", nil // all nodes match
	}

	// Check for a pending replacement: we've launched a new node but haven't
	// drained its counterpart yet. This is true when the total active count
	// exceeds the initial count (each launch adds 1, each drain removes 1).
	totalActive := state.ActiveNodeCount()
	if totalActive > goal.InitialNodeCount && goal.InitialNodeCount > 0 {
		// There's a surplus node — find it (newest node matching goal image)
		replacement := findReplacementNode(state, goal)
		if replacement != nil {
			if queryNodeHealth(replacement.BridgeIP) != nil {
				// New node healthy — wait for golden image, then mark old node for drain
				if !isGoldenReady(replacement.BridgeIP) {
					return false, fmt.Sprintf("Waiting for golden image on %s", replacement.ID), nil
				}
				state.SetNodeStatus(oldNode.ID, "upgrading")
				state.Save()
				return false, fmt.Sprintf("New node %s healthy, marked %s for drain", replacement.ID, oldNode.ID), nil
			}
			// Not healthy yet — wait
			return false, fmt.Sprintf("Waiting for new node %s to become healthy", replacement.ID), nil
		}
	}

	// No pending replacement — launch one
	newEntry, err := launchReplacementNode(cfg, state, goal)
	if err != nil {
		return false, "", fmt.Errorf("launching replacement for %s: %w", oldNode.ID, err)
	}
	return false, fmt.Sprintf("Launched replacement node %s for %s", newEntry.ID, oldNode.ID), nil
}

// reconcileOrchUpgrade handles one step of orchestrator upgrade.
func reconcileOrchUpgrade(cfg HostConfig, state *cluster.State, goal *cluster.UpgradeGoal) (done bool, action string, err error) {
	oldOrch := state.Orchestrator

	// Step A: Assign temp IP if not yet set
	if goal.NewOrchIP == "" {
		newNum := state.NextNodeNum() + 100
		newOctet := cfg.NodeIPOffset + newNum
		goal.NewOrchIP = fmt.Sprintf("%s.%d", cfg.NodeSubnet, newOctet)
		goal.NewOrchTAP = "tap-orch-new"
		goal.NewOrchMAC = fmt.Sprintf("52:54:00:00:01:%02x", newOctet%256)
		state.Save()
		return false, fmt.Sprintf("Assigned temp IP %s for new orchestrator", goal.NewOrchIP), nil
	}

	newDisk := fmt.Sprintf("%s/orchestrator-new.qcow2", cfg.ImagesDir)
	orchISO := fmt.Sprintf("%s/orchestrator-new-cloud-init.iso", cfg.ImagesDir)

	// Step B: Launch new orchestrator if not running
	newOrchHealthy := isOrchHealthy(goal.NewOrchIP)
	if !newOrchHealthy {
		// Check if there's already a QEMU process for orchestrator-new
		// by looking for the disk file — if it doesn't exist, we haven't launched yet
		if !fileExists(newDisk) {
			// Mark old orch as upgrading so health monitor won't restart it after we stop it
			state.SetNodeStatus("orchestrator", "upgrading")

			// Generate cloud-init ISO
			if err := generateCloudInitISOWithOutput(cfg, "orchestrator", "", orchISO,
				"CLOUD_INIT_IP="+goal.NewOrchIP,
				"CLOUD_INIT_MAC="+goal.NewOrchMAC,
			); err != nil {
				state.SetNodeStatus("orchestrator", "active")
				state.Save()
				return false, "", fmt.Errorf("generating cloud-init ISO: %w", err)
			}

			// Create disk
			if err := createCOWDisk(goal.OrchBasePath, newDisk, cfg.OrchestratorDisk); err != nil {
				state.SetNodeStatus("orchestrator", "active")
				state.Save()
				return false, "", fmt.Errorf("creating orchestrator disk: %w", err)
			}

			currentUser, _ := user.Current()
			username := "root"
			if currentUser != nil {
				username = currentUser.Username
			}
			if err := bridge.EnsureTAP(goal.NewOrchTAP, cfg.BridgeDevice, username); err != nil {
				os.Remove(newDisk)
				state.SetNodeStatus("orchestrator", "active")
				state.Save()
				return false, "", fmt.Errorf("creating TAP: %w", err)
			}

			pid, err := qemu.Launch(qemu.VMConfig{
				Name: "orchestrator-new",
				VCPU: cfg.OrchestratorVCPU,
				RAM:  cfg.OrchestratorRAM,
				Disk: newDisk,
				ISO:  orchISO,
				TAP:  goal.NewOrchTAP,
				MAC:  goal.NewOrchMAC,
			}, cfg.ImagesDir)
			if err != nil {
				os.Remove(newDisk)
				state.SetNodeStatus("orchestrator", "active")
				state.Save()
				return false, "", fmt.Errorf("launching new orchestrator: %w", err)
			}
			_ = pid
			state.Save()
			return false, fmt.Sprintf("Launched new orchestrator at %s", goal.NewOrchIP), nil
		}

		// Disk exists but not healthy yet — wait
		return false, fmt.Sprintf("Waiting for new orchestrator at %s to become healthy", goal.NewOrchIP), nil
	}

	// Step C: New orchestrator is healthy — trigger migration if old is still running
	if oldOrch != nil && qemu.IsRunning(oldOrch.PID) {
		migrateReq := map[string]string{
			"source_addr": fmt.Sprintf("%s:8801", oldOrch.BridgeIP),
			"source_ip":   oldOrch.BridgeIP,
		}
		migrateData, _ := json.Marshal(migrateReq)

		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Post(
			fmt.Sprintf("http://%s:8801/api/migrate", goal.NewOrchIP),
			"application/json",
			bytes.NewReader(migrateData),
		)
		if err != nil {
			return false, "", fmt.Errorf("migration request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			return false, "", fmt.Errorf("migration failed (HTTP %d): %s", resp.StatusCode, string(body))
		}

		// Wait for old orchestrator to stop
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if !qemu.IsRunning(oldOrch.PID) {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if qemu.IsRunning(oldOrch.PID) {
			sshKey := findClusterSSHKey(cfg)
			if sshKey != "" {
				exec.Command("ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
					"-o", "ConnectTimeout=5", "-i", sshKey, fmt.Sprintf("ubuntu@%s", oldOrch.BridgeIP),
					"sudo tailscale logout").Run()
			}
			time.Sleep(2 * time.Second)
			qemu.Stop("orchestrator", oldOrch.PID)
		}

		return false, "Migration complete, old orchestrator stopped", nil
	}

	// Step D: Old orchestrator is gone — finalize swap
	// Find the PID of the new orchestrator by scanning for its disk
	newPID := findQEMUPID(newDisk)

	oldDisk := ""
	if oldOrch != nil {
		oldDisk = oldOrch.Disk
	}

	state.SetOrchestrator(cluster.VMEntry{
		ID:           "orchestrator",
		BridgeIP:     goal.NewOrchIP,
		Disk:         newDisk,
		ISO:          orchISO,
		PID:          newPID,
		VCPU:         cfg.OrchestratorVCPU,
		RAM:          cfg.OrchestratorRAM,
		TAP:          goal.NewOrchTAP,
		MAC:          goal.NewOrchMAC,
		ImageVersion: goal.OrchImage.Version,
		ImageCommit:  goal.OrchImage.Commit,
		ImageDigest:  goal.OrchImage.Digest,
	})
	// Clear orch-specific fields from goal
	goal.NewOrchIP = ""
	goal.NewOrchTAP = ""
	goal.NewOrchMAC = ""
	state.Save()

	if oldDisk != "" && oldDisk != newDisk {
		os.Remove(oldDisk)
	}

	return true, fmt.Sprintf("Orchestrator upgraded (PID %d)", newPID), nil
}

// --- Reconciler helpers ---

// firstNodeNotMatchingGoal returns the first active node whose image doesn't
// match the goal. Returns nil if all nodes match.
func firstNodeNotMatchingGoal(state *cluster.State, goal *cluster.UpgradeGoal) *cluster.VMEntry {
	for _, n := range state.Nodes {
		if n.IsActive() && !n.MatchesImage(goal.NodeImage) {
			cp := n
			return &cp
		}
	}
	return nil
}

// findReplacementNode returns a node that has the goal's image version.
// Used to detect an already-launched replacement that may still be booting.
func findReplacementNode(state *cluster.State, goal *cluster.UpgradeGoal) *cluster.VMEntry {
	for _, n := range state.Nodes {
		if n.IsActive() && n.MatchesImage(goal.NodeImage) {
			cp := n
			return &cp
		}
	}
	return nil
}

// orchNeedsUpgrade returns true if the orchestrator's image doesn't match the goal.
func orchNeedsUpgrade(state *cluster.State, goal *cluster.UpgradeGoal) bool {
	if state.Orchestrator == nil || goal.OrchImage == nil {
		return false
	}
	return !state.Orchestrator.MatchesImage(goal.OrchImage)
}

// pullAndDecompress pulls an OCI image and decompresses it to a base QCOW2.
func pullAndDecompress(cfg HostConfig, vmType, tag string) (*cluster.ImageRef, string, error) {
	ctx := context.Background()
	meta, outputFile, err := oci.Pull(ctx, oci.PullOptions{
		Registry:   cfg.OCIRegistry,
		Repository: cfg.OCIRepository,
		VMType:     vmType,
		Tag:        tag,
		OutputDir:  cfg.ImagesDir,
		Auth:       cfg.ociAuth(),
	})
	if err != nil {
		return nil, "", err
	}

	baseName := fmt.Sprintf("%s-base.qcow2", vmType)
	basePath := filepath.Join(cfg.ImagesDir, baseName)

	if outputFile != "" && filepath.Ext(outputFile) == ".zst" {
		if err := decompressZstd(outputFile, basePath); err != nil {
			return nil, "", fmt.Errorf("decompression: %w", err)
		}
		os.Remove(outputFile)
	} else if outputFile != "" {
		os.Rename(outputFile, basePath)
	}

	ref := &cluster.ImageRef{
		Version: meta.Version,
		Commit:  meta.Commit,
		Digest:  meta.Digest,
	}
	return ref, basePath, nil
}

// launchReplacementNode provisions and launches a new node from the upgrade goal's base image.
func launchReplacementNode(cfg HostConfig, state *cluster.State, goal *cluster.UpgradeGoal) (*cluster.VMEntry, error) {
	newNum := state.NextNodeNum()
	newID := fmt.Sprintf("boxcutter-node-%d", newNum)
	newOctet := cfg.NodeIPOffset + newNum
	newBridgeIP := fmt.Sprintf("%s.%d", cfg.NodeSubnet, newOctet)
	newTAP := fmt.Sprintf("tap-node%d", newNum)
	newMAC := fmt.Sprintf("52:54:00:00:00:%02x", newOctet)
	newDisk := fmt.Sprintf("%s/%s.qcow2", cfg.ImagesDir, newID)
	newISO := fmt.Sprintf("%s/%s-cloud-init.iso", cfg.ImagesDir, newID)

	if err := createCOWDisk(goal.NodeBasePath, newDisk, cfg.NodeDisk); err != nil {
		return nil, fmt.Errorf("creating disk: %w", err)
	}

	if !fileExists(newISO) {
		if err := generateCloudInitISO(cfg, "node", newID); err != nil {
			os.Remove(newDisk)
			return nil, fmt.Errorf("generating cloud-init ISO: %w", err)
		}
	}

	currentUser, _ := user.Current()
	username := "root"
	if currentUser != nil {
		username = currentUser.Username
	}

	if err := bridge.EnsureTAP(newTAP, cfg.BridgeDevice, username); err != nil {
		os.Remove(newDisk)
		return nil, fmt.Errorf("creating TAP: %w", err)
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
		os.Remove(newDisk)
		return nil, fmt.Errorf("launching QEMU: %w", err)
	}

	entry := cluster.VMEntry{
		ID:           newID,
		BridgeIP:     newBridgeIP,
		Disk:         newDisk,
		ISO:          newISO,
		PID:          pid,
		VCPU:         cfg.NodeVCPU,
		RAM:          cfg.NodeRAM,
		TAP:          newTAP,
		MAC:          newMAC,
		ImageVersion: goal.NodeImage.Version,
		ImageCommit:  goal.NodeImage.Commit,
		ImageDigest:  goal.NodeImage.Digest,
	}
	state.AddNode(entry)
	state.Save()

	return &entry, nil
}

// isGoldenReady returns true if the node's golden image is available (non-blocking).
func isGoldenReady(bridgeIP string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:8800/api/golden/versions", bridgeIP))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var versions struct {
		Versions []string `json:"versions"`
	}
	json.NewDecoder(resp.Body).Decode(&versions)
	return len(versions.Versions) > 0
}

// isOrchHealthy returns true if an orchestrator is responding at the given bridge IP.
func isOrchHealthy(bridgeIP string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:8801/healthz", bridgeIP))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// findQEMUPID scans /proc for a QEMU process using the given disk path.
func findQEMUPID(diskPath string) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		if strings.Contains(string(cmdline), diskPath) && strings.Contains(string(cmdline), "qemu-system") {
			return pid
		}
	}
	return 0
}

// bootstrapGolden attempts to pull the golden image during bootstrap.
// Non-fatal: if no golden image exists in OCI, it triggers a build on the node instead.
func bootstrapGolden(cfg HostConfig, state *cluster.State) {
	if state.Orchestrator == nil || len(state.Nodes) == 0 {
		log.Println("  Skipping golden image (no orchestrator or nodes)")
		return
	}

	// First check if golden image exists in OCI
	orchAddr := fmt.Sprintf("http://%s:8801", state.Orchestrator.BridgeIP)
	client := &http.Client{Timeout: 10 * time.Second}

	setHeadReq := map[string]string{"version": "latest"}
	data, _ := json.Marshal(setHeadReq)
	resp, err := client.Post(orchAddr+"/api/golden/head", "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("  Could not set golden head on orchestrator: %v", err)
		log.Println("  Golden image will need to be set up manually:")
		log.Println("    make publish TYPE=golden   # or: ssh ubuntu@<node> sudo /var/lib/boxcutter/golden/docker-to-ext4.sh")
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 300 {
		log.Printf("  Orchestrator returned %d for golden head — golden image may not be published yet", resp.StatusCode)
		log.Println("  Triggering golden image build on node-1...")

		// Fall back to building golden on the node directly
		nodeIP := state.Nodes[0].BridgeIP
		sshKey := findClusterSSHKey(cfg)
		if sshKey == "" {
			log.Println("  No cluster SSH key found, cannot trigger remote build")
			return
		}
		buildCmd := exec.Command("ssh",
			"-i", sshKey,
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
			fmt.Sprintf("ubuntu@%s", nodeIP),
			"sudo /var/lib/boxcutter/golden/docker-to-ext4.sh")
		buildCmd.Stdout = os.Stdout
		buildCmd.Stderr = os.Stderr
		if err := buildCmd.Run(); err != nil {
			log.Printf("  Golden image build failed: %v", err)
			log.Println("  You can build it manually later: ssh ubuntu@<node> sudo /var/lib/boxcutter/golden/docker-to-ext4.sh")
		} else {
			log.Println("  Golden image built successfully on node-1")
		}
		return
	}

	log.Println("  Golden head set to 'latest' — nodes will pull via MQTT")

	// Wait for nodes to have the golden image
	log.Println("  Waiting for nodes to build/pull golden image...")
	allReady := true
	for _, n := range state.Nodes {
		if !qemu.IsRunning(n.PID) {
			continue
		}
		if !waitForGoldenReady(n.BridgeIP, 15*time.Minute) {
			log.Printf("  Warning: timeout waiting for golden image on %s", n.ID)
			allReady = false
		}
	}
	if allReady {
		log.Println("  All nodes have golden image")
	}
}

// findClusterSSHKey returns the path to the cluster SSH key.
func findClusterSSHKey(cfg HostConfig) string {
	candidates := []string{
		filepath.Join(cfg.RepoDir, ".boxcutter", "secrets", "cluster-ssh.key"),
	}
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil {
			candidates = append(candidates, filepath.Join(u.HomeDir, ".boxcutter", "secrets", "cluster-ssh.key"))
		}
	}
	candidates = append(candidates, "/etc/boxcutter/secrets/cluster-ssh.key")
	for _, p := range candidates {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// upgradeGolden tells the orchestrator to set a new golden head version.
// Nodes subscribed to MQTT will pull it automatically.
func upgradeGolden(cfg HostConfig, state *cluster.State, tag string) {
	fmt.Printf("\n--- Upgrading golden image to %s ---\n", tag)

	if state.Orchestrator == nil {
		fmt.Fprintln(os.Stderr, "No orchestrator in cluster state")
		os.Exit(1)
	}

	orchAddr := fmt.Sprintf("http://%s:8801", state.Orchestrator.BridgeIP)

	// Set the golden head version on the orchestrator
	setHeadReq := map[string]string{"version": tag}
	data, _ := json.Marshal(setHeadReq)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(orchAddr+"/api/golden/head", "application/json", bytes.NewReader(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to set golden head: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Failed to set golden head (HTTP %d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	fmt.Printf("  Golden head set to %s on orchestrator\n", tag)
	fmt.Printf("  Nodes will pull the new version via MQTT notification\n")

	// Poll nodes until they all have the new golden version
	fmt.Printf("  Waiting for nodes to pull golden image...\n")
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		allReady := true
		for _, n := range state.Nodes {
			if !qemu.IsRunning(n.PID) {
				continue
			}
			health := queryNodeHealth(n.BridgeIP)
			if health == nil {
				allReady = false
				continue
			}
			// Check if node has the golden version via its API
			nodeClient := &http.Client{Timeout: 3 * time.Second}
			resp, err := nodeClient.Get(fmt.Sprintf("http://%s:8800/api/golden/%s", n.BridgeIP, tag))
			if err != nil || resp.StatusCode != 200 {
				allReady = false
				if resp != nil {
					resp.Body.Close()
				}
				continue
			}
			resp.Body.Close()
		}
		if allReady {
			fmt.Printf("  All nodes have golden image %s\n", tag)
			return
		}
		time.Sleep(10 * time.Second)
	}
	fmt.Printf("  Warning: timeout waiting for all nodes to pull golden image\n")
}

// waitForHealth polls a URL until it returns 200 or timeout expires.
func waitForHealth(url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(5 * time.Second)
	}
	return false
}

// generateCloudInitISO calls provision.sh --from-image to create a cloud-init ISO.
// For nodes, name should be like "boxcutter-node-2". For orchestrator, name is ignored.
func generateCloudInitISO(cfg HostConfig, vmType, name string) error {
	return generateCloudInitISOWithOutput(cfg, vmType, name, "")
}

// generateCloudInitISOWithOutput is like generateCloudInitISO but allows specifying
// the output path, IP, and MAC for the cloud-init ISO.
func generateCloudInitISOWithOutput(cfg HostConfig, vmType, name, outputPath string, envOverrides ...string) error {
	script := hostFile(cfg, "provision.sh")
	args := []string{script, vmType}
	if name != "" {
		args = append(args, name)
	}
	args = append(args, "--from-image")
	cmd := exec.Command("bash", args...)
	cmd.Dir = cfg.RepoDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env := os.Environ()
	env = append(env, "BOXCUTTER_IMAGES_DIR="+cfg.ImagesDir)
	if outputPath != "" {
		env = append(env, "CLOUD_INIT_OUTPUT="+outputPath)
	}
	env = append(env, envOverrides...)
	cmd.Env = env
	return cmd.Run()
}

// cliVersion shows the current and latest available image versions.
func cliVersion() {
	cfg := defaultConfig()

	state, err := cluster.Load(cfg.StatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Loading cluster state: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("boxcutter-host: %s\n\n", version)
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
			Auth:       cfg.ociAuth(),
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
	pushOnly := false
	tag := ""
	for i, arg := range os.Args {
		if arg == "--push" {
			push = true
		}
		if arg == "--push-only" {
			push = true
			pushOnly = true
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

	if !pushOnly {
		// Run the build script
		buildScript := hostFile(cfg, "build-image.sh")
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

	if err := oci.Push(ctx, oci.PushOptions{
		Registry:   cfg.OCIRegistry,
		Repository: cfg.OCIRepository,
		VMType:     vmType,
		Tags:       tags,
		FilePath:   imagePath,
		Version:    version,
		Commit:     commit,
		Auth:       cfg.ociAuth(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Push failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Pushed %s image with tags: %v\n", vmType, tags)
}

// cliPushGolden fetches the current golden image from a node, compresses it, and pushes to ghcr.io.
//
//	boxcutter-host push-golden [--tag TAG]
func cliPushGolden() {
	cfg := defaultConfig()

	tag := "latest"
	for i, arg := range os.Args {
		if arg == "--tag" && i+1 < len(os.Args) {
			tag = os.Args[i+1]
		}
	}

	state, err := cluster.Load(cfg.StatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Loading cluster state: %v\n", err)
		os.Exit(1)
	}

	// Find a running node to get the golden image from
	var nodeIP string
	for _, n := range state.Nodes {
		if qemu.IsRunning(n.PID) {
			nodeIP = n.BridgeIP
			break
		}
	}
	if nodeIP == "" {
		fmt.Fprintln(os.Stderr, "No running nodes to fetch golden image from")
		os.Exit(1)
	}

	// Get the current golden version from the node
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:8800/api/golden/versions", nodeIP))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to query node golden versions: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var versions struct {
		Head     string   `json:"head"`
		Versions []string `json:"versions"`
	}
	json.NewDecoder(resp.Body).Decode(&versions)

	if versions.Head == "" {
		fmt.Fprintln(os.Stderr, "Node has no golden head version")
		os.Exit(1)
	}

	fmt.Printf("Fetching golden image %s from node %s...\n", versions.Head, nodeIP)

	// Stream the golden image from the node via SSH + zstd compression
	os.MkdirAll(cfg.ImagesDir, 0755)
	zstPath := filepath.Join(cfg.ImagesDir, fmt.Sprintf("golden-%s.ext4.zst", versions.Head))

	// Use SSH to stream-compress the golden image from the node
	sshCmd := fmt.Sprintf("ssh -i %s/host/ssh-key -o StrictHostKeyChecking=no ubuntu@%s "+
		"'sudo zstd -c --sparse /var/lib/boxcutter/golden/%s.ext4' > %s",
		cfg.RepoDir, nodeIP, versions.Head, zstPath)
	fmt.Printf("  Compressing and downloading...\n")
	cmd := exec.Command("sh", "-c", sshCmd)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Fallback: try with ~/.ssh/id_rsa
		sshCmd2 := fmt.Sprintf("ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no ubuntu@%s "+
			"'sudo zstd -c --sparse /var/lib/boxcutter/golden/%s.ext4' > %s",
			nodeIP, versions.Head, zstPath)
		cmd2 := exec.Command("sh", "-c", sshCmd2)
		cmd2.Stderr = os.Stderr
		if err := cmd2.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to fetch golden image: %v\n", err)
			os.Exit(1)
		}
	}

	fi, _ := os.Stat(zstPath)
	fmt.Printf("  Compressed: %d MB\n", fi.Size()/(1024*1024))

	// Push to ghcr.io
	if tag == "latest" {
		tag = versions.Head
	}
	fmt.Printf("Pushing golden image (tag: %s)...\n", tag)
	ctx := context.Background()
	tags := []string{tag}

	if err := oci.Push(ctx, oci.PushOptions{
		Registry:   cfg.OCIRegistry,
		Repository: cfg.OCIRepository,
		VMType:     "golden",
		Tags:       tags,
		FilePath:   zstPath,
		Version:    tag,
		Auth:       cfg.ociAuth(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Push failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Pushed golden image %s to %s/%s/golden:%s\n", versions.Head, cfg.OCIRegistry, cfg.OCIRepository, tag)
}

// cliRecover scans for running QEMU VMs and rebuilds cluster.json.
// Use this when cluster.json is lost/corrupted but VMs are still running.
func cliRecover() {
	cfg := defaultConfig()

	state, err := cluster.Load(cfg.StatePath)
	if err != nil {
		// If corrupted, start fresh
		log.Printf("Could not load existing state (%v), starting fresh", err)
		state, _ = cluster.Load("/dev/null/nonexistent") // force empty state
	}

	fmt.Println("Scanning for running QEMU VMs...")
	discoverOrphanedVMs(cfg, state)

	// Print what we found
	if state.Orchestrator != nil {
		running := qemu.IsRunning(state.Orchestrator.PID)
		status := "stopped"
		if running {
			status = "running"
		}
		fmt.Printf("  Orchestrator: %s (IP %s, PID %d, %s)\n",
			state.Orchestrator.ID, state.Orchestrator.BridgeIP, state.Orchestrator.PID, status)
	}
	for _, n := range state.Nodes {
		running := qemu.IsRunning(n.PID)
		status := "stopped"
		if running {
			status = "running"
		}
		fmt.Printf("  Node: %s (IP %s, PID %d, %s)\n", n.ID, n.BridgeIP, n.PID, status)
	}

	if state.Orchestrator == nil && len(state.Nodes) == 0 {
		fmt.Println("No QEMU VMs found.")
		return
	}

	state.Save()
	fmt.Printf("\nState saved to %s\n", cfg.StatePath)
	fmt.Println("You can now run: boxcutter-host run")
}

const githubRepo = "AndrewBudd/boxcutter"

// cliSelfUpdate downloads and installs the latest stable boxcutter-host deb package
// from GitHub Releases, then restarts the systemd service.
//
//	boxcutter-host self-update [--version TAG]
func cliSelfUpdate() {
	targetVersion := ""
	for i := 2; i < len(os.Args); i++ {
		if (os.Args[i] == "--version" || os.Args[i] == "--tag") && i+1 < len(os.Args) {
			targetVersion = os.Args[i+1]
			i++
		}
	}

	type ghAsset struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	}
	type ghRelease struct {
		TagName    string    `json:"tag_name"`
		Prerelease bool      `json:"prerelease"`
		Draft      bool      `json:"draft"`
		Assets     []ghAsset `json:"assets"`
	}

	client := &http.Client{Timeout: 30 * time.Second}

	makeReq := func(url string) *http.Request {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		// Use token if available for rate limits / private repos
		token := os.Getenv("GITHUB_TOKEN")
		if token == "" {
			token = os.Getenv("GH_TOKEN")
		}
		if token == "" {
			if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
				token = strings.TrimSpace(string(out))
			}
		}
		if token != "" {
			req.Header.Set("Authorization", "token "+token)
		}
		return req
	}

	var release ghRelease

	if targetVersion != "" {
		// Fetch specific release by tag
		url := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", githubRepo, targetVersion)
		resp, err := client.Do(makeReq(url))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to query GitHub: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			fmt.Fprintf(os.Stderr, "Release %s not found (HTTP %d)\n", targetVersion, resp.StatusCode)
			os.Exit(1)
		}
		json.NewDecoder(resp.Body).Decode(&release)
	} else {
		// Find latest stable (non-prerelease, non-draft) release
		url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=20", githubRepo)
		resp, err := client.Do(makeReq(url))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to query GitHub: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			fmt.Fprintf(os.Stderr, "Failed to list releases (HTTP %d)\n", resp.StatusCode)
			os.Exit(1)
		}
		var releases []ghRelease
		json.NewDecoder(resp.Body).Decode(&releases)

		found := false
		for _, r := range releases {
			if !r.Prerelease && !r.Draft {
				release = r
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "No stable release found. Use --version to specify a pre-release.\n")
			os.Exit(1)
		}
	}

	// Check if already running this version
	if release.TagName == version {
		fmt.Printf("Already running %s\n", version)
		return
	}

	// Find the .deb asset
	var debURL string
	var debName string
	for _, a := range release.Assets {
		if strings.HasSuffix(a.Name, "_amd64.deb") {
			debURL = a.BrowserDownloadURL
			debName = a.Name
			break
		}
	}
	if debURL == "" {
		fmt.Fprintf(os.Stderr, "No .deb package found in release %s\n", release.TagName)
		os.Exit(1)
	}

	fmt.Printf("Updating boxcutter-host: %s -> %s\n", version, release.TagName)
	fmt.Printf("Downloading %s...\n", debName)

	// Download the .deb
	debPath := filepath.Join(os.TempDir(), debName)
	resp, err := client.Get(debURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Download failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	f, err := os.Create(debPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create temp file: %v\n", err)
		os.Exit(1)
	}
	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Download failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Downloaded %s (%d bytes)\n", debName, written)

	// Install the .deb
	fmt.Println("Installing...")
	installCmd := exec.Command("dpkg", "-i", debPath)
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Installation failed: %v\n", err)
		os.Exit(1)
	}

	// Clean up
	os.Remove(debPath)

	// Restart the service
	fmt.Println("Restarting boxcutter-host service...")
	restartCmd := exec.Command("systemctl", "restart", "boxcutter-host")
	restartCmd.Stdout = os.Stdout
	restartCmd.Stderr = os.Stderr
	if err := restartCmd.Run(); err != nil {
		// Don't fail hard — the binary is already installed
		fmt.Printf("Warning: could not restart service: %v\n", err)
		fmt.Println("You may need to restart manually: systemctl restart boxcutter-host")
	}

	fmt.Printf("Updated to %s\n", release.TagName)
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

// waitForGoldenReady polls a node's health endpoint until golden_ready is true.
func waitForGoldenReady(bridgeIP string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		health := queryNodeHealth(bridgeIP)
		if health != nil {
			if ready, ok := health["golden_ready"].(bool); ok && ready {
				return true
			}
		}
		time.Sleep(5 * time.Second)
	}
	return false
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

// startMosquitto launches the MQTT broker as a subprocess.
func startMosquitto(cfg HostConfig) *exec.Cmd {
	confPath := hostFile(cfg, "mosquitto.conf")
	if !fileExists(confPath) {
		log.Printf("mosquitto config not found at %s, skipping MQTT broker", confPath)
		return nil
	}

	// Ensure persistence directory exists
	os.MkdirAll("/var/lib/mosquitto", 0755)

	cmd := exec.Command("mosquitto", "-c", confPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("WARNING: mosquitto failed to start: %v (MQTT features disabled)", err)
		return nil
	}
	log.Printf("mosquitto broker started (PID %d)", cmd.Process.Pid)
	return cmd
}

// ensureBaseImage pulls and decompresses a base image from OCI if it doesn't already exist.
// Retries up to 3 times on network failure.
// buildFromSource runs provision.sh to build binaries, create VM disks and cloud-init ISOs
// from the local repo. After this, phases 3 and 4 of bootstrap are no-ops since the files
// already exist.
func buildFromSource(cfg HostConfig, vmType string) error {
	args := []string{hostFile(cfg, "provision.sh"), vmType}
	if vmType == "node" {
		args = append(args, "boxcutter-node-1")
	}
	args = append(args, "--rebuild")

	log.Printf("  Building %s from source...", vmType)
	cmd := exec.Command("bash", args...)
	cmd.Dir = cfg.RepoDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "BOXCUTTER_IMAGES_DIR="+cfg.ImagesDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("provision.sh %s: %w", vmType, err)
	}
	return nil
}

func ensureBaseImage(cfg HostConfig, vmType string, tag string) (string, *oci.ImageMetadata, error) {
	basePath := filepath.Join(cfg.ImagesDir, fmt.Sprintf("%s-base.qcow2", vmType))

	if fileExists(basePath) {
		log.Printf("  %s base image already exists", vmType)
		return basePath, nil, nil
	}

	var meta *oci.ImageMetadata
	var outputFile string
	var err error

	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			// Clean up any partial download from previous attempt
			zstPath := filepath.Join(cfg.ImagesDir, fmt.Sprintf("%s-image.qcow2.zst", vmType))
			os.Remove(zstPath)
			delay := time.Duration(attempt*5) * time.Second
			log.Printf("  Retrying in %s (attempt %d/3)...", delay, attempt)
			time.Sleep(delay)
		}

		log.Printf("  Pulling %s image from OCI registry (tag: %s)...", vmType, tag)
		ctx := context.Background()
		meta, outputFile, err = oci.Pull(ctx, oci.PullOptions{
			Registry:   cfg.OCIRegistry,
			Repository: cfg.OCIRepository,
			VMType:     vmType,
			Tag:        tag,
			OutputDir:  cfg.ImagesDir,
			Auth:       cfg.ociAuth(),
		})
		if err == nil {
			break
		}
		log.Printf("  Pull failed: %v", err)
	}
	if err != nil {
		return "", nil, fmt.Errorf("pull %s image after 3 attempts: %w", vmType, err)
	}

	if outputFile != "" && filepath.Ext(outputFile) == ".zst" {
		log.Printf("  Decompressing %s image...", vmType)
		if err := decompressZstd(outputFile, basePath); err != nil {
			return "", nil, fmt.Errorf("decompress %s: %w", vmType, err)
		}
		os.Remove(outputFile)
	} else if outputFile != "" {
		os.Rename(outputFile, basePath)
	}

	return basePath, meta, nil
}

func runBootstrap() {
	cfg := defaultConfig()
	os.MkdirAll(cfg.ImagesDir, 0755)

	// Parse bootstrap flags
	fromSource := false
	imageTag := ""
	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--from-source":
			fromSource = true
		case "--version", "--tag":
			if i+1 < len(os.Args) {
				i++
				imageTag = os.Args[i]
			}
		}
	}

	if imageTag == "" && !fromSource && version != "dev" {
		imageTag = version // default: match host binary version
	}
	if imageTag == "" {
		imageTag = "latest"
	}

	if fromSource {
		log.Printf("boxcutter-host bootstrap (from source: %s)", cfg.RepoDir)
	} else {
		log.Printf("boxcutter-host bootstrap (images: %s)", imageTag)
	}

	// Phase 1: Bridge
	log.Println("--- Setting up bridge network ---")
	if err := bridge.Setup(bridge.Config{
		BridgeDevice: cfg.BridgeDevice,
		BridgeIP:     cfg.BridgeIP,
		BridgeCIDR:   cfg.BridgeCIDR,
		HostNIC:      cfg.HostNIC,
	}); err != nil {
		log.Fatalf("Bridge setup: %v", err)
	}

	// Phase 2-4: Get images, create disks, generate ISOs
	var orchMeta, nodeMeta *oci.ImageMetadata
	orchDisk := filepath.Join(cfg.ImagesDir, "orchestrator.qcow2")
	node1Disk := filepath.Join(cfg.ImagesDir, "boxcutter-node-1.qcow2")
	orchISO := filepath.Join(cfg.ImagesDir, "orchestrator-cloud-init.iso")
	node1ISO := filepath.Join(cfg.ImagesDir, "boxcutter-node-1-cloud-init.iso")

	if fromSource {
		// provision.sh builds binaries, creates COW disks, and generates ISOs
		log.Println("--- Building from source ---")
		if err := buildFromSource(cfg, "orchestrator"); err != nil {
			log.Fatalf("Orchestrator build: %v", err)
		}
		if err := buildFromSource(cfg, "node"); err != nil {
			log.Fatalf("Node build: %v", err)
		}
	} else {
		// Pull pre-built OCI images, create COW disks, generate config-only ISOs
		log.Println("--- Ensuring base images ---")
		orchBasePath, orchM, err := ensureBaseImage(cfg, "orchestrator", imageTag)
		if err != nil {
			log.Fatalf("Orchestrator image: %v", err)
		}
		orchMeta = orchM
		nodeBasePath, nodeM, err := ensureBaseImage(cfg, "node", imageTag)
		if err != nil {
			log.Fatalf("Node image: %v", err)
		}
		nodeMeta = nodeM

		log.Println("--- Creating VM disks ---")
		if !fileExists(orchDisk) {
			log.Println("  Creating orchestrator COW disk...")
			if err := createCOWDisk(orchBasePath, orchDisk, cfg.OrchestratorDisk); err != nil {
				log.Fatalf("Orchestrator disk: %v", err)
			}
		} else {
			log.Println("  Orchestrator disk already exists")
		}

		if !fileExists(node1Disk) {
			log.Println("  Creating node-1 COW disk...")
			if err := createCOWDisk(nodeBasePath, node1Disk, cfg.NodeDisk); err != nil {
				log.Fatalf("Node-1 disk: %v", err)
			}
		} else {
			log.Println("  Node-1 disk already exists")
		}

		log.Println("--- Generating cloud-init ISOs ---")
		if !fileExists(orchISO) {
			log.Println("  Generating orchestrator ISO...")
			if err := generateCloudInitISO(cfg, "orchestrator", ""); err != nil {
				log.Fatalf("Orchestrator ISO: %v", err)
			}
		} else {
			log.Println("  Orchestrator ISO already exists")
		}

		if !fileExists(node1ISO) {
			log.Println("  Generating node-1 ISO...")
			if err := generateCloudInitISO(cfg, "node", "boxcutter-node-1"); err != nil {
				log.Fatalf("Node-1 ISO: %v", err)
			}
		} else {
			log.Println("  Node-1 ISO already exists")
		}
	}

	// Phase 5: Launch VMs
	log.Println("--- Launching VMs ---")
	state, _ := cluster.Load(cfg.StatePath)

	currentUser, _ := user.Current()
	username := "root"
	if currentUser != nil {
		username = currentUser.Username
	}

	// Orchestrator
	orchEntry := cluster.VMEntry{
		ID:       "orchestrator",
		Type:     "orchestrator",
		BridgeIP: cfg.OrchestratorIP,
		Disk:     orchDisk,
		ISO:      orchISO,
		VCPU:     cfg.OrchestratorVCPU,
		RAM:      cfg.OrchestratorRAM,
		TAP:      cfg.OrchestratorTAP,
		MAC:      cfg.OrchestratorMAC,
	}
	if orchMeta != nil {
		orchEntry.ImageVersion = orchMeta.Version
		orchEntry.ImageCommit = orchMeta.Commit
		orchEntry.ImageDigest = orchMeta.Digest
	}

	if state.Orchestrator != nil && qemu.IsRunning(state.Orchestrator.PID) {
		log.Printf("  Orchestrator already running (PID %d)", state.Orchestrator.PID)
	} else {
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
		orchEntry.PID = orchPID
		log.Printf("  Orchestrator started (PID %d)", orchPID)
	}
	state.SetOrchestrator(orchEntry)

	// Node-1
	node1BridgeIP := fmt.Sprintf("%s.3", cfg.NodeSubnet)
	node1MAC := "52:54:00:00:00:03"
	node1Entry := cluster.VMEntry{
		ID:       "boxcutter-node-1",
		Type:     "node",
		BridgeIP: node1BridgeIP,
		Disk:     node1Disk,
		ISO:      node1ISO,
		VCPU:     cfg.NodeVCPU,
		RAM:      cfg.NodeRAM,
		TAP:      "tap-node1",
		MAC:      node1MAC,
	}
	if nodeMeta != nil {
		node1Entry.ImageVersion = nodeMeta.Version
		node1Entry.ImageCommit = nodeMeta.Commit
		node1Entry.ImageDigest = nodeMeta.Digest
	}

	alreadyRunning := false
	for _, n := range state.Nodes {
		if n.ID == "boxcutter-node-1" && qemu.IsRunning(n.PID) {
			log.Printf("  Node-1 already running (PID %d)", n.PID)
			node1Entry.PID = n.PID
			alreadyRunning = true
			break
		}
	}
	if !alreadyRunning {
		bridge.EnsureTAP("tap-node1", cfg.BridgeDevice, username)
		node1PID, err := qemu.Launch(qemu.VMConfig{
			Name: "boxcutter-node-1",
			VCPU: cfg.NodeVCPU,
			RAM:  cfg.NodeRAM,
			Disk: node1Disk,
			ISO:  node1ISO,
			TAP:  "tap-node1",
			MAC:  node1MAC,
		}, cfg.ImagesDir)
		if err != nil {
			log.Fatalf("Node-1 launch: %v", err)
		}
		node1Entry.PID = node1PID
		log.Printf("  Node-1 started (PID %d)", node1PID)
	}
	// Clear old nodes and add fresh
	state.Nodes = nil
	state.AddNode(node1Entry)
	state.Save()

	// Phase 6: Wait for health
	// From-source builds need much longer: cloud-init installs packages from scratch
	healthTimeout := 120 * time.Second
	if fromSource {
		healthTimeout = 600 * time.Second
	}
	log.Printf("--- Waiting for VMs to become healthy (timeout: %s) ---", healthTimeout)
	if !waitForHealth(fmt.Sprintf("http://%s:8801/healthz", cfg.OrchestratorIP), healthTimeout) {
		log.Printf("WARNING: Orchestrator did not become healthy within %s", healthTimeout)
	} else {
		log.Println("  Orchestrator healthy")
	}

	if !waitForNodeHealth(node1BridgeIP, healthTimeout) {
		log.Printf("WARNING: Node-1 did not become healthy within %s", healthTimeout)
	} else {
		log.Println("  Node-1 healthy")
	}

	// Phase 7: Golden image — start MQTT broker and trigger golden pull
	log.Println("--- Setting up golden image ---")
	mosquittoCmd := startMosquitto(cfg)

	// Give MQTT a moment to start and nodes to connect
	time.Sleep(5 * time.Second)

	bootstrapGolden(cfg, state)

	if mosquittoCmd != nil {
		mosquittoCmd.Process.Kill()
	}

	log.Printf("Bootstrap complete. State saved to %s", cfg.StatePath)
}
