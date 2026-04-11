package inference

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/config"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/middleware"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/registry"
)

const (
	providerLocal      = "local"
	providerOpenRouter = "openrouter"

	openRouterBaseURL = "https://openrouter.ai/api"
)

// Handler handles inference proxy requests for VMs.
type Handler struct {
	cfg config.InferenceGlobalConfig
	reg *registry.Registry
}

// NewHandler creates a new inference proxy handler.
func NewHandler(cfg config.InferenceGlobalConfig, reg *registry.Registry) *Handler {
	return &Handler{cfg: cfg, reg: reg}
}

// Register adds inference routes to the mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", h.handleProxy)
	mux.HandleFunc("POST /v1/completions", h.handleProxy)
	mux.HandleFunc("GET /v1/models", h.handleModels)
	mux.HandleFunc("GET /inference/config", h.handleConfig)
}

// getInferenceConfig extracts the inference config for the calling VM.
// Returns nil if the VM has no inference config or uses anthropic provider.
func (h *Handler) getInferenceConfig(r *http.Request) (*registry.InferenceConfig, *registry.VMRecord, error) {
	rec, ok := middleware.VMFromContext(r.Context())
	if !ok {
		return nil, nil, fmt.Errorf("no VM context")
	}

	if rec.AgentConfig == nil || rec.AgentConfig.Inference == nil {
		return nil, rec, nil
	}

	inf := rec.AgentConfig.Inference
	if inf.Provider == "anthropic" || inf.Provider == "" {
		return nil, rec, nil
	}

	return inf, rec, nil
}

// upstreamURL returns the base URL for the given provider.
func (h *Handler) upstreamURL(provider string) (string, error) {
	switch provider {
	case providerLocal:
		return h.cfg.LocalEndpoint, nil
	case providerOpenRouter:
		return openRouterBaseURL, nil
	default:
		return "", fmt.Errorf("unsupported inference provider: %s", provider)
	}
}

// handleProxy reverse-proxies /v1/chat/completions and /v1/completions.
func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request) {
	infCfg, rec, err := h.getInferenceConfig(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if infCfg == nil {
		http.Error(w, "inference not configured for this VM", http.StatusNotFound)
		return
	}

	upstream, err := h.upstreamURL(infCfg.Provider)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	target, err := url.Parse(upstream)
	if err != nil {
		http.Error(w, "invalid upstream URL", http.StatusInternalServerError)
		return
	}

	vmName := rec.VMID
	log.Printf("inference: proxying %s %s for VM %s to %s", r.Method, r.URL.Path, vmName, upstream)

	// Check if this is a streaming request by peeking at the body.
	// We need to read the body to check, then pass it along.
	isStreaming := false
	if r.Body != nil {
		bodyBytes, readErr := io.ReadAll(r.Body)
		if readErr == nil {
			// Check for "stream": true in the JSON body
			var body map[string]interface{}
			if json.Unmarshal(bodyBytes, &body) == nil {
				if stream, ok := body["stream"]; ok {
					if b, ok := stream.(bool); ok && b {
						isStreaming = true
					}
				}
			}
			// Restore body for the proxy
			r.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
			r.ContentLength = int64(len(bodyBytes))
		}
	}

	if isStreaming {
		h.handleStreamingProxy(w, r, target, infCfg, vmName)
		return
	}

	// Non-streaming: use standard httputil.ReverseProxy
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Path stays the same — /v1/chat/completions or /v1/completions
			req.Header.Set("X-Forwarded-For", r.RemoteAddr)

			if infCfg.Provider == providerOpenRouter && infCfg.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+infCfg.APIKey)
			}
		},
	}

	proxy.ServeHTTP(w, r)
}

// handleStreamingProxy handles SSE streaming responses by flushing chunks immediately.
func (h *Handler) handleStreamingProxy(w http.ResponseWriter, r *http.Request, target *url.URL, infCfg *registry.InferenceConfig, vmName string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Build upstream request
	upstreamURL := *target
	upstreamURL.Path = r.URL.Path
	upstreamURL.RawQuery = r.URL.RawQuery

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), r.Body)
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}

	// Copy relevant headers
	proxyReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	proxyReq.Header.Set("Accept", r.Header.Get("Accept"))
	proxyReq.Header.Set("X-Forwarded-For", r.RemoteAddr)
	proxyReq.Host = target.Host

	if infCfg.Provider == providerOpenRouter && infCfg.APIKey != "" {
		proxyReq.Header.Set("Authorization", "Bearer "+infCfg.APIKey)
	}

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		log.Printf("inference: upstream error for VM %s: %v", vmName, err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	// Stream the response body, flushing after each chunk
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				log.Printf("inference: write error for VM %s: %v", vmName, writeErr)
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Printf("inference: read error for VM %s: %v", vmName, readErr)
			}
			return
		}
	}
}

// handleModels returns the configured model for the calling VM in OpenAI-compatible format.
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	infCfg, _, err := h.getInferenceConfig(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if infCfg == nil {
		http.Error(w, "inference not configured for this VM", http.StatusNotFound)
		return
	}

	// Return OpenAI-compatible model list
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data": []map[string]interface{}{
			{
				"id":       infCfg.Model,
				"object":   "model",
				"owned_by": infCfg.Provider,
			},
		},
	})
}

// handleConfig returns the raw inference configuration for the calling VM.
func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	infCfg, _, err := h.getInferenceConfig(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if infCfg == nil {
		http.Error(w, "inference not configured for this VM", http.StatusNotFound)
		return
	}

	// Return config without the API key
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"provider": infCfg.Provider,
		"model":    infCfg.Model,
	})
}
