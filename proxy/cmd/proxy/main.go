package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/elazarl/goproxy"
)

var (
	listenAddr    = flag.String("listen", ":8080", "proxy listen address")
	caCertPath    = flag.String("ca-cert", "/etc/boxcutter/ca.crt", "CA certificate for MITM")
	caKeyPath     = flag.String("ca-key", "/etc/boxcutter/ca.key", "CA key for MITM")
	vmidSocket    = flag.String("vmid-socket", "/run/vmid/admin.sock", "vmid admin socket")
	allowlistPath = flag.String("allowlist", "/etc/boxcutter/proxy-allowlist.conf", "egress allowlist for paranoid mode")
)

func main() {
	flag.Parse()

	// Load CA for MITM
	caCert, err := tls.LoadX509KeyPair(*caCertPath, *caKeyPath)
	if err != nil {
		log.Fatalf("loading CA keypair: %v", err)
	}
	ca, err := x509.ParseCertificate(caCert.Certificate[0])
	if err != nil {
		log.Fatalf("parsing CA cert: %v", err)
	}

	// Load allowlist
	allowlist := loadAllowlist(*allowlistPath)

	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false

	// Set up MITM CA
	goproxyCa := caCert
	goproxyCa.Leaf = ca
	goproxy.GoproxyCa = goproxyCa

	// MITM all HTTPS
	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)

	// Swap sentinel tokens in requests
	proxy.OnRequest().DoFunc(func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		// Check allowlist for paranoid mode
		if len(allowlist) > 0 {
			host := r.URL.Hostname()
			if !isAllowed(host, allowlist) {
				return r, goproxy.NewResponse(r, goproxy.ContentTypeText,
					http.StatusForbidden, "blocked by proxy allowlist")
			}
		}

		// Scan Authorization header for sentinel tokens
		auth := r.Header.Get("Authorization")
		if auth != "" {
			swapped := swapSentinel(auth, *vmidSocket)
			if swapped != auth {
				r.Header.Set("Authorization", swapped)
			}
		}

		return r, nil
	})

	log.Printf("Boxcutter proxy listening on %s", *listenAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, proxy))
}

func swapSentinel(authHeader, socketPath string) string {
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 {
		return authHeader
	}
	prefix := parts[0]
	tokenVal := parts[1]

	real, ok := resolveSentinel(tokenVal, socketPath)
	if !ok {
		return authHeader
	}
	return prefix + " " + real
}

type sentinelResponse struct {
	Token string `json:"token"`
}

func resolveSentinel(sentinel, socketPath string) (string, bool) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}
	resp, err := client.Get(fmt.Sprintf("http://localhost/internal/sentinel/%s", sentinel))
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}

	var sr sentinelResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return "", false
	}
	return sr.Token, sr.Token != ""
}

func loadAllowlist(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var entries []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entries = append(entries, line)
	}
	return entries
}

func isAllowed(host string, allowlist []string) bool {
	for _, pattern := range allowlist {
		if pattern == host {
			return true
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:]
			if strings.HasSuffix(host, suffix) || host == pattern[2:] {
				return true
			}
		}
	}
	return false
}
