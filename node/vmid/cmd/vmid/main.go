package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"unsafe"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/api"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/config"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/middleware"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/registry"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/sentinel"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/token"
)

// markConn wraps a net.Conn with the fwmark read from the socket.
type markConn struct {
	net.Conn
	mark int
}

// markListener reads SO_MARK from each accepted connection.
type markListener struct {
	net.Listener
}

func (ml *markListener) Accept() (net.Conn, error) {
	conn, err := ml.Listener.Accept()
	if err != nil {
		return nil, err
	}
	mark := readSOMark(conn)
	return &markConn{Conn: conn, mark: mark}, nil
}

func readSOMark(conn net.Conn) int {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return 0
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return 0
	}
	var mark int
	raw.Control(func(fd uintptr) {
		val, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			syscall.SOL_SOCKET,
			syscall.SO_MARK,
			uintptr(unsafe.Pointer(&mark)),
			uintptr(unsafe.Pointer(&(&[1]int32{4})[0])),
			0,
		)
		_ = val
		if errno != 0 {
			mark = 0
		}
	})
	return mark
}

func main() {
	configPath := flag.String("config", "/etc/vmid/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	// Registry
	reg := registry.New()

	// Sentinel store
	sentinelStore := sentinel.NewStore()

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
	metaHandler := api.NewMetadataHandler(jwtIssuer, githubMinter, sentinelStore, cfg.Metadata)
	metaHandler.Register(vmMux)
	wingmanHandler := api.NewWingmanHandler(reg)
	wingmanHandler.Register(vmMux)

	identityMiddleware := middleware.Identity(reg)

	// Public endpoints (no identity required)
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("GET /.well-known/jwks.json", metaHandler.HandleJWKS)
	publicMux.HandleFunc("GET /metadata/ssh-keys", metaHandler.HandleSSHKeys)
	publicMux.HandleFunc("GET /metadata/ca-cert", metaHandler.HandleCACert)
	publicMux.Handle("/", identityMiddleware(vmMux))

	// Listen with mark-aware listener on metadata address
	listenAddr := fmt.Sprintf("%s:%d", cfg.Listen.VMAddr, cfg.Listen.VMPort)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listening on %s: %v", listenAddr, err)
	}

	vmServer := &http.Server{
		Handler: publicMux,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if mc, ok := c.(*markConn); ok {
				return middleware.WithMark(ctx, mc.mark)
			}
			return ctx
		},
	}

	// Admin server (Unix socket)
	adminMux := http.NewServeMux()
	adminHandler := api.NewAdminHandler(reg, githubMinter, sentinelStore)
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
		log.Printf("VM metadata server listening on %s", listenAddr)
		if err := vmServer.Serve(&markListener{Listener: ln}); err != http.ErrServerClosed {
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
