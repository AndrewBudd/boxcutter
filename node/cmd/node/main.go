package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/AndrewBudd/boxcutter/node/internal/api"
	"github.com/AndrewBudd/boxcutter/node/internal/config"
	"github.com/AndrewBudd/boxcutter/node/internal/network"
	"github.com/AndrewBudd/boxcutter/node/internal/vm"
	"github.com/AndrewBudd/boxcutter/node/internal/vmid"
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

	// Wait for shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("Shutting down node agent...")
	server.Close()
}
