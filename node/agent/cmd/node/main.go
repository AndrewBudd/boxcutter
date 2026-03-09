package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewBudd/boxcutter/node/agent/internal/api"
	"github.com/AndrewBudd/boxcutter/node/agent/internal/config"
	"github.com/AndrewBudd/boxcutter/node/agent/internal/golden"
	nodemqtt "github.com/AndrewBudd/boxcutter/node/agent/internal/mqtt"
	"github.com/AndrewBudd/boxcutter/node/agent/internal/network"
	"github.com/AndrewBudd/boxcutter/node/agent/internal/vm"
	"github.com/AndrewBudd/boxcutter/node/agent/internal/vmid"
)

func main() {
	configPath := flag.String("config", "/etc/boxcutter/boxcutter.yaml", "path to boxcutter.yaml")
	listenAddr := flag.String("listen", ":8800", "HTTP API listen address")
	skipNetSetup := flag.Bool("skip-net", false, "skip one-time network setup")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	// One-time network infrastructure setup
	if !*skipNetSetup {
		log.Println("Setting up network infrastructure...")
		if err := network.Setup(); err != nil {
			log.Fatalf("network setup: %v", err)
		}
		log.Println("Network ready.")
	}

	// Connect to vmid admin socket
	vmidClient := vmid.NewClient("/run/vmid/admin.sock")
	if vmidClient.Healthy() {
		log.Println("vmid: connected")
	} else {
		log.Println("vmid: not available (will retry on VM operations)")
	}

	hostname, _ := os.Hostname()

	// VM manager
	mgr := vm.NewManager(cfg, vmidClient)

	// HTTP API
	mux := http.NewServeMux()
	handler := api.NewHandler(mgr)
	handler.Register(mux)

	// Health check (separate from VM health)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	server := &http.Server{
		Addr:    *listenAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("Node agent listening on %s", *listenAddr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server: %v", err)
		}
	}()

	// Restart VMs that were running before node restarted
	go mgr.RestartAll()

	// Auto-build golden image if missing
	go autoGoldenBuild(mgr)

	// Golden image manager — handles OCI pulls and version switching
	goldenMgr := golden.NewManager(golden.Config{
		GoldenDir:     filepath.Dir(cfg.Storage.GoldenLocalPath),
		OCIRegistry:   cfg.OCI.Registry,
		OCIRepository: cfg.OCI.Repository,
	})

	// MQTT client — connect to broker on host bridge
	brokerAddr := cfg.MQTT.BrokerAddr
	if brokerAddr == "" {
		brokerAddr = nodemqtt.BrokerAddrFromEnv()
	}
	var mqttClient *nodemqtt.Client
	mqttClient, err = nodemqtt.Connect(nodemqtt.Config{
		BrokerAddr: brokerAddr,
		NodeID:     hostname,
		OnGolden: func(version string) {
			if err := goldenMgr.SetHead(version); err != nil {
				log.Printf("mqtt: failed to set golden head %s: %v", version, err)
				return
			}
			// Publish updated image list
			if mqttClient != nil {
				mqttClient.PublishImages(goldenMgr.Versions())
			}
		},
	})
	if err != nil {
		log.Printf("mqtt: connection failed (non-fatal): %v", err)
	} else {
		defer mqttClient.Close()

		// Periodic status publishing
		go mqttStatusLoop(mgr, mqttClient, goldenMgr)

		// Publish initial image list
		mqttClient.PublishImages(goldenMgr.Versions())
	}

	// Register golden manager with the API handler for golden endpoints
	handler.SetGoldenManager(goldenMgr)

	// Register with orchestrator if configured
	if cfg.Orchestrator.URL != "" {
		go registerWithOrchestrator(cfg, mgr)
	}

	// Wait for shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("Shutting down node agent...")
	server.Close()
}

func autoGoldenBuild(mgr *vm.Manager) {
	goldenPath := mgr.GoldenPath()
	if _, err := os.Stat(goldenPath); err == nil {
		return // golden image exists
	}

	buildScript := filepath.Join(filepath.Dir(goldenPath), "docker-to-ext4.sh")
	if _, err := os.Stat(buildScript); err != nil {
		log.Printf("Golden image missing but no build script at %s", buildScript)
		return
	}

	log.Printf("Golden image missing — starting automatic build...")
	cmd := exec.Command("bash", buildScript)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("Golden image auto-build failed: %v", err)
		return
	}
	log.Printf("Golden image auto-build complete")
}

func registerWithOrchestrator(cfg *config.Config, mgr *vm.Manager) {
	orchURL := strings.TrimRight(cfg.Orchestrator.URL, "/")

	// Get Tailscale IP (non-blocking — start registration with bridge IP immediately)
	var tailscaleIP string
	go func() {
		for {
			out, err := exec.Command("tailscale", "ip", "-4").Output()
			if err == nil {
				ip := strings.TrimSpace(string(out))
				if ip != "" {
					tailscaleIP = ip
					return
				}
			}
			time.Sleep(5 * time.Second)
		}
	}()

	hostname, _ := os.Hostname()
	bridgeIP := cfg.Node.BridgeIP

	// Get system info from manager health
	health := mgr.Health()
	ramTotal, _ := health["ram_total_mib"].(int)
	vcpuTotal, _ := health["vcpu_total"].(int)

	// api_addr uses bridge IP for direct host-local communication
	regBody := map[string]interface{}{
		"id":             hostname,
		"tailscale_name": hostname,
		"tailscale_ip":   tailscaleIP,
		"bridge_ip":      bridgeIP,
		"api_addr":       fmt.Sprintf("%s:8800", bridgeIP),
		"ram_total_mib":  ramTotal,
		"vcpu_total":     vcpuTotal,
	}

	// Retry registration every 10 seconds until successful
	for {
		// Update tailscale IP if it became available
		if tailscaleIP != "" {
			regBody["tailscale_ip"] = tailscaleIP
		}
		data, _ := json.Marshal(regBody)
		resp, err := http.Post(orchURL+"/api/nodes/register", "application/json", bytes.NewReader(data))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				log.Printf("orchestrator: registered as %s (bridge=%s, tailscale=%s)", hostname, bridgeIP, tailscaleIP)
				break
			}
			log.Printf("orchestrator: registration returned %d, retrying...", resp.StatusCode)
		} else {
			log.Printf("orchestrator: registration failed: %v, retrying...", err)
		}
		time.Sleep(10 * time.Second)
	}

	// If Tailscale IP wasn't ready at registration time, re-register once it is
	if tailscaleIP == "" {
		go func() {
			for {
				time.Sleep(5 * time.Second)
				if tailscaleIP != "" {
					regBody["tailscale_ip"] = tailscaleIP
					data, _ := json.Marshal(regBody)
					resp, err := http.Post(orchURL+"/api/nodes/register", "application/json", bytes.NewReader(data))
					if err == nil {
						resp.Body.Close()
						log.Printf("orchestrator: updated registration with tailscale_ip=%s", tailscaleIP)
					}
					return
				}
			}
		}()
	}

	// Start heartbeat loop
	go heartbeatLoop(orchURL, hostname, mgr)
}

func mqttStatusLoop(mgr *vm.Manager, mc *nodemqtt.Client, gm *golden.Manager) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		health := mgr.Health()
		ramTotal, _ := health["ram_total_mib"].(int)
		ramAlloc, _ := health["ram_allocated_mib"].(int)
		vmsRunning, _ := health["vms_running"].(int)
		goldenReady, _ := health["golden_ready"].(bool)
		hostname, _ := health["hostname"].(string)

		mc.PublishStatus(&nodemqtt.NodeStatus{
			Hostname:        hostname,
			RAMTotalMIB:     ramTotal,
			RAMAllocatedMIB: ramAlloc,
			VMsRunning:      vmsRunning,
			GoldenReady:     goldenReady,
			GoldenHead:      gm.CurrentHead(),
			Status:          "active",
		})
	}
}

func heartbeatLoop(orchURL, nodeID string, mgr *vm.Manager) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		hbBody := map[string]interface{}{
			"ram_allocated_mib": mgr.AllocatedRAMMiB(),
			"vms_running":       mgr.RunningVMCount(),
		}
		data, _ := json.Marshal(hbBody)
		url := fmt.Sprintf("%s/api/nodes/%s/heartbeat", orchURL, nodeID)
		resp, err := http.Post(url, "application/json", bytes.NewReader(data))
		if err != nil {
			log.Printf("orchestrator: heartbeat failed: %v", err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("orchestrator: heartbeat returned %d", resp.StatusCode)
		}
	}
}
