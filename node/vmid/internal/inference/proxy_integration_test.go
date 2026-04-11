package inference

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/config"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/middleware"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/registry"
)

// TestIntegration_FullMuxRouting exercises the complete proxy through the
// registered mux (same path as production vmid), not just individual handlers.
// It verifies that Register() wires up the correct routes and that the
// identity middleware correctly maps fwmarks to VM configs.
func TestIntegration_FullMuxRouting(t *testing.T) {
	// Fake Ollama upstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":     "mux-test-1",
				"object": "chat.completion",
				"choices": []map[string]interface{}{
					{"message": map[string]string{"content": "routed correctly"}},
				},
			})
		case "/v1/completions":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":     "mux-completions-1",
				"object": "text_completion",
			})
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	reg := registry.New()

	// VM with local inference (OpenCode worker)
	openCodeVM := &registry.VMRecord{
		VMID: "worker-1",
		IP:   "10.0.0.2",
		Mark: 200,
		Mode: "normal",
		AgentConfig: &registry.AgentConfig{
			Team:  "test-team",
			Agent: "worker",
			Inference: &registry.InferenceConfig{
				Provider: "local",
				Model:    "qwen3-coder:30b",
			},
		},
	}
	reg.Register(openCodeVM)

	// VM with no inference (Claude Code lead)
	claudeVM := &registry.VMRecord{
		VMID: "lead-1",
		IP:   "10.0.0.3",
		Mark: 201,
		Mode: "normal",
		AgentConfig: &registry.AgentConfig{
			Team:  "test-team",
			Agent: "lead",
		},
	}
	reg.Register(claudeVM)

	h := NewHandler(config.InferenceGlobalConfig{
		LocalEndpoint: upstream.URL,
	}, reg)

	// Build full mux (same as main.go)
	mux := http.NewServeMux()
	h.Register(mux)
	identityMW := middleware.Identity(reg)
	server := identityMW(mux)

	t.Run("POST /v1/chat/completions routes to upstream for local VM", func(t *testing.T) {
		body := `{"model":"qwen3-coder:30b","messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ctx := middleware.WithMark(req.Context(), 200)
		req = req.WithContext(ctx)

		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["id"] != "mux-test-1" {
			t.Errorf("expected mux-test-1, got %v", resp["id"])
		}
	})

	t.Run("POST /v1/completions routes to upstream for local VM", func(t *testing.T) {
		body := `{"model":"qwen3-coder:30b","prompt":"function add("}`
		req := httptest.NewRequest("POST", "/v1/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ctx := middleware.WithMark(req.Context(), 200)
		req = req.WithContext(ctx)

		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("GET /v1/models returns configured model", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		ctx := middleware.WithMark(req.Context(), 200)
		req = req.WithContext(ctx)

		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		data := resp["data"].([]interface{})
		model := data[0].(map[string]interface{})
		if model["id"] != "qwen3-coder:30b" {
			t.Errorf("expected model qwen3-coder:30b, got %v", model["id"])
		}
	})

	t.Run("GET /inference/config returns provider and model without API key", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/inference/config", nil)
		ctx := middleware.WithMark(req.Context(), 200)
		req = req.WithContext(ctx)

		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["provider"] != "local" {
			t.Errorf("expected provider local, got %v", resp["provider"])
		}
		if _, ok := resp["api_key"]; ok {
			t.Error("api_key should not be exposed")
		}
	})

	t.Run("Claude Code VM gets 404 on inference endpoints", func(t *testing.T) {
		endpoints := []struct {
			method string
			path   string
			body   string
		}{
			{"POST", "/v1/chat/completions", `{"model":"test","messages":[]}`},
			{"POST", "/v1/completions", `{"prompt":"test"}`},
			{"GET", "/v1/models", ""},
			{"GET", "/inference/config", ""},
		}

		for _, ep := range endpoints {
			var bodyReader io.Reader
			if ep.body != "" {
				bodyReader = strings.NewReader(ep.body)
			}
			req := httptest.NewRequest(ep.method, ep.path, bodyReader)
			if ep.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			ctx := middleware.WithMark(req.Context(), 201) // Claude VM
			req = req.WithContext(ctx)

			rr := httptest.NewRecorder()
			server.ServeHTTP(rr, req)

			if rr.Code != http.StatusNotFound {
				t.Errorf("%s %s for Claude VM: expected 404, got %d", ep.method, ep.path, rr.Code)
			}
		}
	})
}

// TestIntegration_SSEStreamingEndToEnd verifies that SSE chunks from an upstream
// are faithfully forwarded through the proxy with correct headers and content.
func TestIntegration_SSEStreamingEndToEnd(t *testing.T) {
	// Upstream that sends SSE chunks with controlled timing
	chunks := []string{
		`data: {"id":"stream-1","choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"id":"stream-1","choices":[{"delta":{"content":"def "}}]}`,
		`data: {"id":"stream-1","choices":[{"delta":{"content":"hello"}}]}`,
		`data: {"id":"stream-1","choices":[{"delta":{"content":"():"}}]}`,
		`data: {"id":"stream-1","choices":[{"delta":{"content":" return "}}]}`,
		`data: {"id":"stream-1","choices":[{"delta":{"content":"\"hello\""}}]}`,
		`data: [DONE]`,
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("expected flusher support on upstream")
			return
		}

		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			flusher.Flush()
			time.Sleep(5 * time.Millisecond) // Simulate token-by-token generation
		}
	}))
	defer upstream.Close()

	reg := registry.New()
	vm := &registry.VMRecord{
		VMID: "stream-vm",
		IP:   "10.0.0.2",
		Mark: 300,
		Mode: "normal",
		AgentConfig: &registry.AgentConfig{
			Team:  "test-team",
			Agent: "stream-worker",
			Inference: &registry.InferenceConfig{
				Provider: "local",
				Model:    "qwen3-coder:30b",
			},
		},
	}
	reg.Register(vm)

	h := NewHandler(config.InferenceGlobalConfig{
		LocalEndpoint: upstream.URL,
	}, reg)

	mux := http.NewServeMux()
	h.Register(mux)
	identityMW := middleware.Identity(reg)
	server := identityMW(mux)

	body := `{"model":"qwen3-coder:30b","messages":[{"role":"user","content":"write hello"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := middleware.WithMark(req.Context(), 300)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify SSE content type
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("expected Content-Type text/event-stream, got %s", ct)
	}

	// Verify all chunks arrived
	result := rr.Body.String()
	for _, chunk := range chunks {
		if !strings.Contains(result, chunk) {
			t.Errorf("missing chunk in streamed response: %s", chunk)
		}
	}

	// Verify we can parse the SSE events
	scanner := bufio.NewScanner(strings.NewReader(result))
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}

	if len(dataLines) != len(chunks) {
		t.Errorf("expected %d data lines, got %d", len(chunks), len(dataLines))
	}

	// Verify the last event is [DONE]
	if len(dataLines) > 0 && dataLines[len(dataLines)-1] != "[DONE]" {
		t.Errorf("expected last event to be [DONE], got %s", dataLines[len(dataLines)-1])
	}

	// Verify content tokens can be extracted
	var contentParts []string
	for _, dl := range dataLines {
		if dl == "[DONE]" {
			continue
		}
		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(dl), &evt); err != nil {
			continue
		}
		choices, _ := evt["choices"].([]interface{})
		if len(choices) > 0 {
			choice := choices[0].(map[string]interface{})
			delta, _ := choice["delta"].(map[string]interface{})
			if content, ok := delta["content"].(string); ok {
				contentParts = append(contentParts, content)
			}
		}
	}

	reassembled := strings.Join(contentParts, "")
	if reassembled != `def hello(): return "hello"` {
		t.Errorf("reassembled content = %q, want def hello(): return \"hello\"", reassembled)
	}
}

// TestIntegration_MultiVMIsolation verifies that two VMs with different
// inference configs get correctly routed responses and cannot cross-access.
func TestIntegration_MultiVMIsolation(t *testing.T) {
	var mu sync.Mutex
	receivedRequests := make(map[string]string) // path -> X-Forwarded-For

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedRequests[r.Header.Get("X-Forwarded-For")] = r.URL.Path
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     "multi-vm-test",
			"object": "chat.completion",
			"choices": []map[string]interface{}{
				{"message": map[string]string{"content": "ok"}},
			},
		})
	}))
	defer upstream.Close()

	reg := registry.New()

	vm1 := &registry.VMRecord{
		VMID: "worker-a",
		IP:   "10.0.0.10",
		Mark: 400,
		Mode: "normal",
		AgentConfig: &registry.AgentConfig{
			Team:  "team-a",
			Agent: "worker",
			Inference: &registry.InferenceConfig{
				Provider: "local",
				Model:    "qwen3-coder:30b",
			},
		},
	}
	vm2 := &registry.VMRecord{
		VMID: "worker-b",
		IP:   "10.0.0.11",
		Mark: 401,
		Mode: "normal",
		AgentConfig: &registry.AgentConfig{
			Team:  "team-b",
			Agent: "worker",
			Inference: &registry.InferenceConfig{
				Provider: "local",
				Model:    "qwen3-coder:8b",
			},
		},
	}
	vm3 := &registry.VMRecord{
		VMID: "lead-a",
		IP:   "10.0.0.12",
		Mark: 402,
		Mode: "normal",
		AgentConfig: &registry.AgentConfig{
			Team:  "team-a",
			Agent: "lead",
			// No inference config — Claude Code
		},
	}
	reg.Register(vm1)
	reg.Register(vm2)
	reg.Register(vm3)

	h := NewHandler(config.InferenceGlobalConfig{
		LocalEndpoint: upstream.URL,
	}, reg)
	mux := http.NewServeMux()
	h.Register(mux)
	server := middleware.Identity(reg)(mux)

	// VM1 (worker-a) should get proxied
	body := `{"model":"qwen3-coder:30b","messages":[{"role":"user","content":"test"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(middleware.WithMark(req.Context(), 400))
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("VM1: expected 200, got %d", rr.Code)
	}

	// VM2 (worker-b) should also get proxied
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2 = req2.WithContext(middleware.WithMark(req2.Context(), 401))
	rr2 := httptest.NewRecorder()
	server.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("VM2: expected 200, got %d", rr2.Code)
	}

	// VM3 (lead-a, no inference) should get 404
	req3 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req3.Header.Set("Content-Type", "application/json")
	req3 = req3.WithContext(middleware.WithMark(req3.Context(), 402))
	rr3 := httptest.NewRecorder()
	server.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusNotFound {
		t.Fatalf("VM3 (no inference): expected 404, got %d", rr3.Code)
	}

	// Verify /v1/models returns different models per VM
	modelsReq1 := httptest.NewRequest("GET", "/v1/models", nil)
	modelsReq1 = modelsReq1.WithContext(middleware.WithMark(modelsReq1.Context(), 400))
	modelsRR1 := httptest.NewRecorder()
	server.ServeHTTP(modelsRR1, modelsReq1)
	var models1 map[string]interface{}
	json.Unmarshal(modelsRR1.Body.Bytes(), &models1)
	data1 := models1["data"].([]interface{})
	m1 := data1[0].(map[string]interface{})

	modelsReq2 := httptest.NewRequest("GET", "/v1/models", nil)
	modelsReq2 = modelsReq2.WithContext(middleware.WithMark(modelsReq2.Context(), 401))
	modelsRR2 := httptest.NewRecorder()
	server.ServeHTTP(modelsRR2, modelsReq2)
	var models2 map[string]interface{}
	json.Unmarshal(modelsRR2.Body.Bytes(), &models2)
	data2 := models2["data"].([]interface{})
	m2 := data2[0].(map[string]interface{})

	if m1["id"] != "qwen3-coder:30b" {
		t.Errorf("VM1 model = %v, want qwen3-coder:30b", m1["id"])
	}
	if m2["id"] != "qwen3-coder:8b" {
		t.Errorf("VM2 model = %v, want qwen3-coder:8b", m2["id"])
	}
}

// TestIntegration_XForwardedForSet verifies the proxy sets X-Forwarded-For
// on requests to the upstream, which is important for audit logging.
func TestIntegration_XForwardedForSet(t *testing.T) {
	var receivedXFF string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedXFF = r.Header.Get("X-Forwarded-For")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": "xff-test"})
	}))
	defer upstream.Close()

	h, reg, _ := setupTestHandler(t, &registry.InferenceConfig{
		Provider: "local",
		Model:    "test-model",
	}, upstream.URL)

	body := `{"model":"test-model","messages":[{"role":"user","content":"test"}]}`
	req := requestWithIdentity(t, 100, "POST", "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()

	wrapped := serveWithIdentity(reg, h.handleProxy)
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if receivedXFF == "" {
		t.Error("upstream did not receive X-Forwarded-For header")
	}
}

// TestIntegration_LargeStreamingResponse verifies the proxy handles a
// large number of SSE events without truncation or corruption.
func TestIntegration_LargeStreamingResponse(t *testing.T) {
	numChunks := 200
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		for i := 0; i < numChunks; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"large-%d\",\"choices\":[{\"delta\":{\"content\":\"token-%d \"}}]}\n\n", i, i)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	h, reg, _ := setupTestHandler(t, &registry.InferenceConfig{
		Provider: "local",
		Model:    "qwen3-coder:30b",
	}, upstream.URL)

	body := `{"model":"qwen3-coder:30b","messages":[{"role":"user","content":"generate lots"}],"stream":true}`
	req := requestWithIdentity(t, 100, "POST", "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()

	wrapped := serveWithIdentity(reg, h.handleProxy)
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	// Count data lines
	scanner := bufio.NewScanner(strings.NewReader(rr.Body.String()))
	var dataCount int
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			dataCount++
		}
	}

	expected := numChunks + 1 // chunks + [DONE]
	if dataCount != expected {
		t.Errorf("expected %d data events, got %d", expected, dataCount)
	}
}

// TestIntegration_UpstreamError verifies the proxy returns 502 when
// the upstream is unreachable.
func TestIntegration_UpstreamError(t *testing.T) {
	// Use a URL that will fail to connect
	h, reg, _ := setupTestHandler(t, &registry.InferenceConfig{
		Provider: "local",
		Model:    "qwen3-coder:30b",
	}, "http://127.0.0.1:1") // port 1 — guaranteed to fail

	body := `{"model":"qwen3-coder:30b","messages":[{"role":"user","content":"fail"}],"stream":true}`
	req := requestWithIdentity(t, 100, "POST", "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()

	wrapped := serveWithIdentity(reg, h.handleProxy)
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for unreachable upstream, got %d", rr.Code)
	}
}

// TestIntegration_NonStreamingBodyPreserved verifies the proxy correctly
// forwards the request body to the upstream (body must survive the
// stream-detection read + restore).
func TestIntegration_NonStreamingBodyPreserved(t *testing.T) {
	var receivedBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read error: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": "body-test"})
	}))
	defer upstream.Close()

	h, reg, _ := setupTestHandler(t, &registry.InferenceConfig{
		Provider: "local",
		Model:    "qwen3-coder:30b",
	}, upstream.URL)

	originalBody := `{"model":"qwen3-coder:30b","messages":[{"role":"user","content":"preserve this body"}],"temperature":0.7}`
	req := requestWithIdentity(t, 100, "POST", "/v1/chat/completions", strings.NewReader(originalBody))
	rr := httptest.NewRecorder()

	wrapped := serveWithIdentity(reg, h.handleProxy)
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	// Verify the upstream received the complete body (stream-detection
	// reads and restores via io.NopCloser)
	if !bytes.Equal(receivedBody, []byte(originalBody)) {
		t.Errorf("upstream received body = %q, want %q", string(receivedBody), originalBody)
	}
}
