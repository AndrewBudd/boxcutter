package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/config"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/sentinel"
)

func newTestMetadataHandler(t *testing.T, metadata config.MetadataFilesConfig) *MetadataHandler {
	t.Helper()
	return &MetadataHandler{
		sentinel: sentinel.NewStore(),
		metadata: metadata,
	}
}

func TestHandleClaudeCredentials_NotConfigured(t *testing.T) {
	h := newTestMetadataHandler(t, config.MetadataFilesConfig{})

	req := httptest.NewRequest("GET", "/metadata/claude-credentials", nil)
	w := httptest.NewRecorder()
	h.handleClaudeCredentials(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if body := w.Body.String(); body == "" {
		t.Error("expected error message in body")
	}
}

func TestHandleClaudeCredentials_FileMissing(t *testing.T) {
	h := newTestMetadataHandler(t, config.MetadataFilesConfig{
		ClaudeCredentialsPath: "/nonexistent/credentials.json",
	})

	req := httptest.NewRequest("GET", "/metadata/claude-credentials", nil)
	w := httptest.NewRecorder()
	h.handleClaudeCredentials(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleClaudeCredentials_Success(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")
	credData := `{"oauth_refresh_token":"test-refresh-token","oauth_client_id":"test-client"}`
	if err := os.WriteFile(credPath, []byte(credData), 0600); err != nil {
		t.Fatal(err)
	}

	h := newTestMetadataHandler(t, config.MetadataFilesConfig{
		ClaudeCredentialsPath: credPath,
	})

	req := httptest.NewRequest("GET", "/metadata/claude-credentials", nil)
	w := httptest.NewRecorder()
	h.handleClaudeCredentials(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := w.Body.String(); body != credData {
		t.Errorf("body = %q, want %q", body, credData)
	}
}

func TestHandleClaudeCredentials_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")
	credData := `{"oauth_refresh_token":"secret"}`
	os.WriteFile(credPath, []byte(credData), 0600)

	h := newTestMetadataHandler(t, config.MetadataFilesConfig{
		ClaudeCredentialsPath: credPath,
	})

	// Verify the endpoint serves the file correctly
	req := httptest.NewRequest("GET", "/metadata/claude-credentials", nil)
	w := httptest.NewRecorder()
	h.handleClaudeCredentials(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Verify file is read fresh each time (supports rotation)
	newData := `{"oauth_refresh_token":"rotated-token"}`
	os.WriteFile(credPath, []byte(newData), 0600)

	req = httptest.NewRequest("GET", "/metadata/claude-credentials", nil)
	w = httptest.NewRecorder()
	h.handleClaudeCredentials(w, req)

	if body := w.Body.String(); body != newData {
		t.Errorf("after rotation: body = %q, want %q", body, newData)
	}
}

func TestHandleCACert_NotConfigured(t *testing.T) {
	h := newTestMetadataHandler(t, config.MetadataFilesConfig{})

	req := httptest.NewRequest("GET", "/metadata/ca-cert", nil)
	w := httptest.NewRecorder()
	h.handleCACert(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleSSHKeys_Empty(t *testing.T) {
	h := newTestMetadataHandler(t, config.MetadataFilesConfig{})

	req := httptest.NewRequest("GET", "/metadata/ssh-keys", nil)
	w := httptest.NewRecorder()
	h.handleSSHKeys(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestHandleSSHKeys_WithKeys(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "keys")
	os.WriteFile(keyPath, []byte("ssh-ed25519 AAAA user@dev\nssh-rsa BBBB user@laptop\n"), 0600)

	h := newTestMetadataHandler(t, config.MetadataFilesConfig{
		SSHAuthorizedKeys: []string{keyPath},
	})

	req := httptest.NewRequest("GET", "/metadata/ssh-keys", nil)
	w := httptest.NewRecorder()
	h.handleSSHKeys(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if body != "ssh-ed25519 AAAA user@dev\nssh-rsa BBBB user@laptop\n" {
		t.Errorf("body = %q", body)
	}
}

func TestHandleVMSSHKey_NotConfigured(t *testing.T) {
	h := newTestMetadataHandler(t, config.MetadataFilesConfig{})

	req := httptest.NewRequest("GET", "/metadata/boxcutter-ssh-key", nil)
	w := httptest.NewRecorder()
	h.handleVMSSHKey(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
