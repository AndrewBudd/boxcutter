package token

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/config"
	"github.com/AndrewBudd/boxcutter/node/vmid/internal/registry"
)

func writeTestPEM(t *testing.T, dir string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test-key.pem")
	data := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewGitHubTokenMinter_NilConfig(t *testing.T) {
	minter, err := NewGitHubTokenMinter(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if minter != nil {
		t.Fatal("expected nil minter for nil config")
	}
}

func TestNewGitHubTokenMinter_MissingPEM_ReturnsNil(t *testing.T) {
	cfg := &config.GitHubConfig{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPath: "/nonexistent/github-app.pem",
	}
	minter, err := NewGitHubTokenMinter(cfg, nil)
	if err != nil {
		t.Fatalf("missing PEM should return nil minter, not error: %v", err)
	}
	if minter != nil {
		t.Fatal("expected nil minter when PEM file does not exist")
	}
}

func TestNewGitHubTokenMinter_ValidPEM(t *testing.T) {
	dir := t.TempDir()
	pemPath := writeTestPEM(t, dir)

	cfg := &config.GitHubConfig{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPath: pemPath,
	}
	minter, err := NewGitHubTokenMinter(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if minter == nil {
		t.Fatal("expected non-nil minter")
	}
	if minter.appID != 12345 {
		t.Errorf("appID = %d, want 12345", minter.appID)
	}
	if minter.installationID != 67890 {
		t.Errorf("installationID = %d, want 67890", minter.installationID)
	}
}

func TestNewGitHubTokenMinter_InvalidPEM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.pem")
	os.WriteFile(path, []byte("not a PEM file"), 0600)

	cfg := &config.GitHubConfig{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPath: path,
	}
	_, err := NewGitHubTokenMinter(cfg, nil)
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestNewGitHubTokenMinter_EmptyPrivateKeyPath(t *testing.T) {
	cfg := &config.GitHubConfig{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPath: "",
	}
	_, err := NewGitHubTokenMinter(cfg, nil)
	if err == nil {
		t.Fatal("expected error for empty private_key_path")
	}
}

func TestNewGitHubTokenMinter_ZeroAppID(t *testing.T) {
	dir := t.TempDir()
	pemPath := writeTestPEM(t, dir)

	cfg := &config.GitHubConfig{
		AppID:          0,
		InstallationID: 67890,
		PrivateKeyPath: pemPath,
	}
	_, err := NewGitHubTokenMinter(cfg, nil)
	if err == nil {
		t.Fatal("expected error for zero app_id")
	}
}

func TestNewGitHubTokenMinter_ZeroInstallationID(t *testing.T) {
	dir := t.TempDir()
	pemPath := writeTestPEM(t, dir)

	cfg := &config.GitHubConfig{
		AppID:          12345,
		InstallationID: 0,
		PrivateKeyPath: pemPath,
	}
	_, err := NewGitHubTokenMinter(cfg, nil)
	if err == nil {
		t.Fatal("expected error for zero installation_id")
	}
}

func TestNewGitHubTokenMinter_MintAppJWT(t *testing.T) {
	dir := t.TempDir()
	pemPath := writeTestPEM(t, dir)

	cfg := &config.GitHubConfig{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPath: pemPath,
	}
	minter, err := NewGitHubTokenMinter(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	jwt, err := minter.mintAppJWT()
	if err != nil {
		t.Fatalf("mintAppJWT failed: %v", err)
	}
	if jwt == "" {
		t.Error("expected non-empty JWT")
	}
}

func TestResolvePolicy_WithRepos(t *testing.T) {
	dir := t.TempDir()
	pemPath := writeTestPEM(t, dir)

	cfg := &config.GitHubConfig{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPath: pemPath,
	}
	minter, err := NewGitHubTokenMinter(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	rec := &registry.VMRecord{
		VMID:        "test-vm",
		GitHubRepos: []string{"owner/repo1", "owner/repo2"},
	}

	repos, perms, err := minter.ResolvePolicy(rec)
	if err != nil {
		t.Fatalf("ResolvePolicy failed: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("repos = %d, want 2", len(repos))
	}
	if perms["contents"] != "write" {
		t.Errorf("perms[contents] = %q, want write", perms["contents"])
	}
}

func TestResolvePolicy_NoReposNoPolicy(t *testing.T) {
	dir := t.TempDir()
	pemPath := writeTestPEM(t, dir)

	cfg := &config.GitHubConfig{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPath: pemPath,
	}
	minter, err := NewGitHubTokenMinter(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	rec := &registry.VMRecord{
		VMID: "test-vm",
	}

	_, _, err = minter.ResolvePolicy(rec)
	if err == nil {
		t.Fatal("expected error when no repos and no matching policy")
	}
}

func TestResolvePolicy_PolicyMatch(t *testing.T) {
	dir := t.TempDir()
	pemPath := writeTestPEM(t, dir)

	policies := []config.Policy{
		{
			Match: config.PolicyMatch{
				Labels: map[string]string{"env": "dev"},
			},
			GitHub: &config.PolicyGitHub{
				Repositories: []string{"owner/repo1"},
				Permissions:  map[string]string{"contents": "read"},
			},
		},
	}

	cfg := &config.GitHubConfig{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPath: pemPath,
	}
	minter, err := NewGitHubTokenMinter(cfg, policies)
	if err != nil {
		t.Fatal(err)
	}

	rec := &registry.VMRecord{
		VMID:   "test-vm",
		Labels: map[string]string{"env": "dev"},
	}

	repos, perms, err := minter.ResolvePolicy(rec)
	if err != nil {
		t.Fatalf("ResolvePolicy failed: %v", err)
	}
	if len(repos) != 1 || repos[0] != "owner/repo1" {
		t.Errorf("repos = %v, want [owner/repo1]", repos)
	}
	if perms["contents"] != "read" {
		t.Errorf("perms[contents] = %q, want read", perms["contents"])
	}
}

func TestMatchLabels(t *testing.T) {
	tests := []struct {
		name   string
		vm     map[string]string
		match  map[string]string
		expect bool
	}{
		{"empty match matches all", map[string]string{"a": "1"}, map[string]string{}, true},
		{"exact match", map[string]string{"env": "dev"}, map[string]string{"env": "dev"}, true},
		{"no match", map[string]string{"env": "prod"}, map[string]string{"env": "dev"}, false},
		{"subset match", map[string]string{"env": "dev", "team": "x"}, map[string]string{"env": "dev"}, true},
		{"missing label", map[string]string{}, map[string]string{"env": "dev"}, false},
		{"nil vm labels", nil, map[string]string{"env": "dev"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchLabels(tt.vm, tt.match)
			if got != tt.expect {
				t.Errorf("matchLabels(%v, %v) = %v, want %v", tt.vm, tt.match, got, tt.expect)
			}
		})
	}
}
