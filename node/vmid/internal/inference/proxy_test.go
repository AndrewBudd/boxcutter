package inference

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/config"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/middleware"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/registry"
)

// setupTestHandler creates a Handler, Registry, and VMRecord for testing.
func setupTestHandler(t *testing.T, infCfg *registry.InferenceConfig, upstreamURL string) (*Handler, *registry.Registry, *registry.VMRecord) {
	t.Helper()

	reg := registry.New()
	rec := &registry.VMRecord{
		VMID: "test-vm-1",
		IP:   "10.0.0.2",
		Mark: 100,
		Mode: "normal",
		AgentConfig: &registry.AgentConfig{
			Team:  "test-team",
			Agent: "test-agent",
		},
	}
	if infCfg != nil {
		rec.AgentConfig.Inference = infCfg
	}
	reg.Register(rec)

	localEndpoint := "http://192.168.50.1:11434"
	if upstreamURL != "" {
		localEndpoint = upstreamURL
	}

	h := NewHandler(config.InferenceGlobalConfig{
		LocalEndpoint: localEndpoint,
	}, reg)

	return h, reg, rec
}

// requestWithIdentity creates an HTTP request with the fwmark set in context.
func requestWithIdentity(t *testing.T, mark int, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := middleware.WithMark(req.Context(), mark)
	req = req.WithContext(ctx)
	return req
}

// serveWithIdentity wraps a handler function with the identity middleware.
func serveWithIdentity(reg *registry.Registry, handler http.HandlerFunc) http.Handler {
	identityMW := middleware.Identity(reg)
	return identityMW(http.HandlerFunc(handler))
}

func TestProxyLocalProvider(t *testing.T) {
	// Set up a fake Ollama upstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("expected path /v1/chat/completions, got %s", r.URL.Path)
		}
		// Verify no Authorization header for local provider
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected no Authorization header for local provider, got %s", auth)
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff == "" {
			t.Errorf("expected X-Forwarded-For header to be set")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-123",
			"object":  "chat.completion",
			"choices": []map[string]interface{}{{"message": map[string]string{"content": "hello"}}},
		})
	}))
	defer upstream.Close()

	h, reg, _ := setupTestHandler(t, &registry.InferenceConfig{
		Provider: "local",
		Model:    "qwen3-coder:30b",
	}, upstream.URL)

	body := `{"model":"qwen3-coder:30b","messages":[{"role":"user","content":"hi"}]}`
	req := requestWithIdentity(t, 100, "POST", "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()

	wrapped := serveWithIdentity(reg, h.handleProxy)
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if resp["id"] != "chatcmpl-123" {
		t.Errorf("unexpected response id: %v", resp["id"])
	}
}

func TestProxyOpenRouterInjectsAuth(t *testing.T) {
	// Set up a fake upstream that records the Authorization header
	var receivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-789\"}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	reg := registry.New()
	rec := &registry.VMRecord{
		VMID: "test-vm-or",
		IP:   "10.0.0.3",
		Mark: 101,
		Mode: "normal",
		AgentConfig: &registry.AgentConfig{
			Team:  "test-team",
			Agent: "test-agent",
			Inference: &registry.InferenceConfig{
				Provider: "openrouter",
				Model:    "deepseek/deepseek-coder",
				APIKey:   "sk-or-test-key-123",
			},
		},
	}
	reg.Register(rec)

	infCfg := rec.AgentConfig.Inference

	// Test handleStreamingProxy directly with target pointing to our test server.
	// This bypasses the URL resolution (which maps "openrouter" to the real URL)
	// and lets us verify auth header injection.
	target, _ := url.Parse(upstream.URL)

	h := NewHandler(config.InferenceGlobalConfig{LocalEndpoint: "http://localhost:1"}, reg)

	body := `{"model":"deepseek/deepseek-coder","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.handleStreamingProxy(rr, req, target, infCfg, "test-vm-or")

	if receivedAuth != "Bearer sk-or-test-key-123" {
		t.Errorf("expected Authorization header 'Bearer sk-or-test-key-123', got '%s'", receivedAuth)
	}

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSSEStreamingForwarded(t *testing.T) {
	// Set up a fake upstream that returns SSE chunks
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher support")
		}

		chunks := []string{
			`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":" world"}}]}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	h, reg, _ := setupTestHandler(t, &registry.InferenceConfig{
		Provider: "local",
		Model:    "qwen3-coder:30b",
	}, upstream.URL)

	body := `{"model":"qwen3-coder:30b","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := requestWithIdentity(t, 100, "POST", "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()

	wrapped := serveWithIdentity(reg, h.handleProxy)
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	responseBody := rr.Body.String()
	if !strings.Contains(responseBody, "Hello") {
		t.Errorf("expected streamed response to contain 'Hello', got: %s", responseBody)
	}
	if !strings.Contains(responseBody, "[DONE]") {
		t.Errorf("expected streamed response to contain '[DONE]', got: %s", responseBody)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("expected Content-Type text/event-stream, got %s", ct)
	}
}

func TestNoInferenceConfigReturns404(t *testing.T) {
	// VM with no inference config
	h, reg, _ := setupTestHandler(t, nil, "")

	body := `{"model":"anything","messages":[{"role":"user","content":"hi"}]}`
	req := requestWithIdentity(t, 100, "POST", "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()

	wrapped := serveWithIdentity(reg, h.handleProxy)
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAnthropicProviderReturns404(t *testing.T) {
	// VM with anthropic provider should get 404 (Claude Code doesn't use proxy)
	h, reg, _ := setupTestHandler(t, &registry.InferenceConfig{
		Provider: "anthropic",
		Model:    "claude-sonnet-4-20250514",
	}, "")

	body := `{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}]}`
	req := requestWithIdentity(t, 100, "POST", "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()

	wrapped := serveWithIdentity(reg, h.handleProxy)
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for anthropic provider, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestModelsEndpoint(t *testing.T) {
	h, reg, _ := setupTestHandler(t, &registry.InferenceConfig{
		Provider: "local",
		Model:    "qwen3-coder:30b",
	}, "")

	req := requestWithIdentity(t, 100, "GET", "/v1/models", nil)
	rr := httptest.NewRecorder()

	wrapped := serveWithIdentity(reg, h.handleModels)
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if resp["object"] != "list" {
		t.Errorf("expected object 'list', got %v", resp["object"])
	}

	data, ok := resp["data"].([]interface{})
	if !ok || len(data) == 0 {
		t.Fatal("expected non-empty data array")
	}

	model := data[0].(map[string]interface{})
	if model["id"] != "qwen3-coder:30b" {
		t.Errorf("expected model id 'qwen3-coder:30b', got %v", model["id"])
	}
	if model["owned_by"] != "local" {
		t.Errorf("expected owned_by 'local', got %v", model["owned_by"])
	}
}

func TestModelsEndpointNoConfig(t *testing.T) {
	h, reg, _ := setupTestHandler(t, nil, "")

	req := requestWithIdentity(t, 100, "GET", "/v1/models", nil)
	rr := httptest.NewRecorder()

	wrapped := serveWithIdentity(reg, h.handleModels)
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestConfigEndpoint(t *testing.T) {
	h, reg, _ := setupTestHandler(t, &registry.InferenceConfig{
		Provider: "openrouter",
		Model:    "deepseek/deepseek-coder",
		APIKey:   "sk-secret",
	}, "")

	req := requestWithIdentity(t, 100, "GET", "/inference/config", nil)
	rr := httptest.NewRecorder()

	wrapped := serveWithIdentity(reg, h.handleConfig)
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if resp["provider"] != "openrouter" {
		t.Errorf("expected provider 'openrouter', got %v", resp["provider"])
	}
	if resp["model"] != "deepseek/deepseek-coder" {
		t.Errorf("expected model 'deepseek/deepseek-coder', got %v", resp["model"])
	}
	// API key should NOT be exposed
	if _, ok := resp["api_key"]; ok {
		t.Errorf("API key should not be exposed in config endpoint")
	}
}
