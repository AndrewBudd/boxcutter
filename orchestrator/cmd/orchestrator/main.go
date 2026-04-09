package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/api"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/config"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/db"
	orchmqtt "github.com/AndrewBudd/boxcutter/orchestrator/internal/mqtt"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/state"
)

func main() {
	configPath := flag.String("config", "/etc/boxcutter/boxcutter.yaml", "path to boxcutter.yaml")
	listenAddr := flag.String("listen", ":8801", "HTTP API listen address")
	dbPath := flag.String("db", "", "SQLite database path (overrides config)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	if *dbPath != "" {
		cfg.DB.Path = *dbPath
	}

	// Open database (only for SSH keys and golden config)
	database, err := db.Open(cfg.DB.Path)
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}
	defer database.Close()

	// In-memory state store (nodes, VMs, golden images — rebuilt on startup)
	store := state.New()

	// MQTT client
	mqttClient, err := orchmqtt.Connect(orchmqtt.Config{
		BrokerAddr: orchmqtt.BrokerAddrFromEnv(),
	})
	if err != nil {
		log.Printf("mqtt: connection failed (non-fatal): %v", err)
	} else {
		defer mqttClient.Close()
	}

	// HTTP API
	mux := http.NewServeMux()
	handler := api.NewHandler(database, store)
	handler.SetMQTT(mqttClient)
	handler.Register(mux)

	server := &http.Server{
		Addr:    *listenAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("Orchestrator listening on %s (stateless mode)", *listenAddr)
		log.Printf("Database: %s (SSH keys + golden config only)", cfg.DB.Path)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server: %v", err)
		}
	}()

	// Wait for shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("Shutting down orchestrator...")
	server.Close()
}
