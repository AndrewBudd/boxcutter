package token

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AndrewBudd/boxcutter/vmid/internal/config"
	"github.com/AndrewBudd/boxcutter/vmid/internal/registry"
	"github.com/golang-jwt/jwt/v5"
)

type GitHubTokenMinter struct {
	appID          int64
	installationID int64 // default installation
	privateKey     interface{} // *rsa.PrivateKey
	policies       []config.Policy

	mu                  sync.RWMutex
	repoCache           []string
	repoCacheTime       time.Time
	repoCacheTTL        time.Duration
	installationCache   map[string]int64 // owner -> installation_id
}

type GitHubTokenResponse struct {
	Token        string            `json:"token"`
	ExpiresAt    string            `json:"expires_at"`
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

func NewGitHubTokenMinter(cfg *config.GitHubConfig, policies []config.Policy) (*GitHubTokenMinter, error) {
	if cfg == nil {
		return nil, nil
	}

	data, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading GitHub App private key: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", cfg.PrivateKeyPath)
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing GitHub App RSA key: %w", err)
	}

	return &GitHubTokenMinter{
		appID:             cfg.AppID,
		installationID:    cfg.InstallationID,
		privateKey:        key,
		policies:          policies,
		repoCacheTTL:      cfg.RepoCacheTTL,
		installationCache: make(map[string]int64),
	}, nil
}

func (g *GitHubTokenMinter) mintAppJWT() (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(), // clock skew
		"exp": now.Add(10 * time.Minute).Unix(),
		"iss": g.appID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(g.privateKey)
}

func (g *GitHubTokenMinter) getInstallationRepos(appJWT string) ([]string, error) {
	g.mu.RLock()
	if time.Since(g.repoCacheTime) < g.repoCacheTTL && len(g.repoCache) > 0 {
		repos := g.repoCache
		g.mu.RUnlock()
		return repos, nil
	}
	g.mu.RUnlock()

	url := fmt.Sprintf("https://api.github.com/installation/repositories?per_page=100")
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")

	// Need an installation token first to list repos
	instToken, err := g.createInstallationToken(appJWT, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("getting installation token for repo list: %w", err)
	}

	req.Header.Set("Authorization", "token "+instToken.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing installation repos: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("listing repos: %d %s", resp.StatusCode, body)
	}

	var result struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding repo list: %w", err)
	}

	repos := make([]string, len(result.Repositories))
	for i, r := range result.Repositories {
		repos[i] = r.FullName
	}

	g.mu.Lock()
	g.repoCache = repos
	g.repoCacheTime = time.Now()
	g.mu.Unlock()

	return repos, nil
}

type installationTokenRequest struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

type installationTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// findInstallationForRepo looks up which installation has access to a given repo.
// Uses GET /repos/{owner}/{repo}/installation with App JWT auth.
// Caches by owner since all repos under one owner use the same installation.
func (g *GitHubTokenMinter) findInstallationForRepo(appJWT, repoFullName string) (int64, error) {
	parts := strings.SplitN(repoFullName, "/", 2)
	if len(parts) != 2 {
		return g.installationID, nil
	}
	owner := parts[0]

	g.mu.RLock()
	if id, ok := g.installationCache[owner]; ok {
		g.mu.RUnlock()
		return id, nil
	}
	g.mu.RUnlock()

	url := fmt.Sprintf("https://api.github.com/repos/%s/installation", repoFullName)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "vmid")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("looking up installation for %s: %w", repoFullName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("looking up installation for %s: %d %s", repoFullName, resp.StatusCode, body)
	}

	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decoding installation for %s: %w", repoFullName, err)
	}

	g.mu.Lock()
	g.installationCache[owner] = result.ID
	g.mu.Unlock()

	return result.ID, nil
}

func (g *GitHubTokenMinter) createInstallationToken(appJWT string, repos []string, perms map[string]string) (*installationTokenResponse, error) {
	// Determine which installation to use
	installID := g.installationID
	if len(repos) > 0 {
		// Look up the correct installation for the first repo's owner
		id, err := g.findInstallationForRepo(appJWT, repos[0])
		if err == nil && id != 0 {
			installID = id
		}
	}

	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installID)

	body := &installationTokenRequest{}
	if len(repos) > 0 {
		// GitHub API wants repo names without org prefix
		shortNames := make([]string, len(repos))
		for i, r := range repos {
			parts := strings.SplitN(r, "/", 2)
			if len(parts) == 2 {
				shortNames[i] = parts[1]
			} else {
				shortNames[i] = r
			}
		}
		body.Repositories = shortNames
	}
	if len(perms) > 0 {
		body.Permissions = perms
	}

	bodyBytes, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(bodyBytes)))
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("creating installation token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("creating installation token: %d %s", resp.StatusCode, respBody)
	}

	var result installationTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding installation token: %w", err)
	}

	return &result, nil
}

// ResolvePolicy finds the first matching policy for a VM record.
// If the VM was registered with a github_repo, that takes precedence.
func (g *GitHubTokenMinter) ResolvePolicy(rec *registry.VMRecord) (repos []string, perms map[string]string, err error) {
	// On-the-fly policy from provisioner — dev VMs need full repo access
	if rec.GitHubRepo != "" {
		return []string{rec.GitHubRepo}, map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			"issues":        "write",
			"metadata":      "read",
		}, nil
	}

	// Policy-based resolution
	for _, p := range g.policies {
		if matchLabels(rec.Labels, p.Match.Labels) && p.GitHub != nil {
			resolved, err := g.resolveRepoGlobs(p.GitHub.Repositories)
			if err != nil {
				return nil, nil, err
			}
			return resolved, p.GitHub.Permissions, nil
		}
	}

	return nil, nil, fmt.Errorf("no GitHub policy matches vm %s", rec.VMID)
}

func (g *GitHubTokenMinter) resolveRepoGlobs(patterns []string) ([]string, error) {
	hasGlob := false
	for _, p := range patterns {
		if strings.Contains(p, "*") {
			hasGlob = true
			break
		}
	}
	if !hasGlob {
		return patterns, nil
	}

	appJWT, err := g.mintAppJWT()
	if err != nil {
		return nil, err
	}
	allRepos, err := g.getInstallationRepos(appJWT)
	if err != nil {
		return nil, err
	}

	var matched []string
	for _, repo := range allRepos {
		for _, pattern := range patterns {
			ok, _ := filepath.Match(pattern, repo)
			if ok {
				matched = append(matched, repo)
				break
			}
		}
	}
	return matched, nil
}

func matchLabels(vmLabels, matchLabels map[string]string) bool {
	for k, v := range matchLabels {
		if vmLabels[k] != v {
			return false
		}
	}
	return true
}

// MintToken creates a scoped GitHub installation token for a VM.
func (g *GitHubTokenMinter) MintToken(rec *registry.VMRecord) (*GitHubTokenResponse, error) {
	repos, perms, err := g.ResolvePolicy(rec)
	if err != nil {
		return nil, err
	}

	appJWT, err := g.mintAppJWT()
	if err != nil {
		return nil, fmt.Errorf("minting app JWT: %w", err)
	}

	instToken, err := g.createInstallationToken(appJWT, repos, perms)
	if err != nil {
		return nil, err
	}

	return &GitHubTokenResponse{
		Token:        instToken.Token,
		ExpiresAt:    instToken.ExpiresAt,
		Repositories: repos,
		Permissions:  perms,
	}, nil
}
