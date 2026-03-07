package oci

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// GitHubAppAuth holds credentials for authenticating to ghcr.io via a GitHub App.
type GitHubAppAuth struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPath string
}

// Token generates a ghcr.io-compatible token from GitHub App credentials.
// Returns a short-lived installation token that can be used as a password for ghcr.io.
func (a *GitHubAppAuth) Token() (string, error) {
	data, err := os.ReadFile(a.PrivateKeyPath)
	if err != nil {
		return "", fmt.Errorf("reading GitHub App key: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("no PEM block found in %s", a.PrivateKeyPath)
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing RSA key: %w", err)
	}

	// Create App JWT
	now := time.Now()
	claims := jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
		"iss": a.AppID,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	appJWT, err := tok.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("signing JWT: %w", err)
	}

	// Exchange for installation token
	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", a.InstallationID)
	body := `{"permissions":{"packages":"write"}}`
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("creating installation token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("installation token request failed (%d): %s", resp.StatusCode, respBody)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}

	return result.Token, nil
}
