package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/AndrewBudd/boxcutter/vmid/internal/api"
	"github.com/AndrewBudd/boxcutter/vmid/internal/config"
	"github.com/AndrewBudd/boxcutter/vmid/internal/middleware"
	"github.com/AndrewBudd/boxcutter/vmid/internal/registry"
	"github.com/AndrewBudd/boxcutter/vmid/internal/token"
)

func main() {
	configPath := flag.String("config", "/etc/vmid/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	// Registry
	reg := registry.New()

	// JWT issuer (always enabled)
	jwtIssuer, err := token.NewJWTIssuer(cfg.JWT.KeyPath, cfg.JWT.TTL)
	if err != nil {
		log.Fatalf("initializing JWT issuer: %v", err)
	}

	// GitHub token minter (optional)
	githubMinter, err := token.NewGitHubTokenMinter(cfg.GitHub, cfg.Policies)
	if err != nil {
		log.Fatalf("initializing GitHub token minter: %v", err)
	}
	if githubMinter != nil {
		log.Println("GitHub App integration enabled")
	}

	// VM-facing server (listens on the metadata IP)
	vmMux := http.NewServeMux()
	metaHandler := api.NewMetadataHandler(jwtIssuer, githubMinter)
	metaHandler.Register(vmMux)

	identityMiddleware := middleware.Identity(reg)

	// JWKS is public (needed by token verifiers outside VMs)
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("GET /.well-known/jwks.json", metaHandler.HandleJWKS)
	publicMux.Handle("/", identityMiddleware(vmMux))

	vmServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Listen.VMPort),
		Handler: publicMux,
	}

	// Admin server (Unix socket)
	adminMux := http.NewServeMux()
	adminHandler := api.NewAdminHandler(reg, githubMinter)
	adminHandler.Register(adminMux)

	// Health endpoint on admin socket
	adminMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	// Clean up stale socket
	os.Remove(cfg.Listen.AdminSocket)

	adminListener, err := net.Listen("unix", cfg.Listen.AdminSocket)
	if err != nil {
		log.Fatalf("listening on admin socket %s: %v", cfg.Listen.AdminSocket, err)
	}
	// Make socket accessible to boxcutter-ctl
	os.Chmod(cfg.Listen.AdminSocket, 0666)

	adminServer := &http.Server{
		Handler: adminMux,
	}

	// Start servers
	go func() {
		log.Printf("VM metadata server listening on :%d", cfg.Listen.VMPort)
		if err := vmServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("VM server error: %v", err)
		}
	}()

	go func() {
		log.Printf("Admin server listening on %s", cfg.Listen.AdminSocket)
		if err := adminServer.Serve(adminListener); err != http.ErrServerClosed {
			log.Fatalf("Admin server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("Shutting down...")
	vmServer.Close()
	adminServer.Close()
	os.Remove(cfg.Listen.AdminSocket)
}
