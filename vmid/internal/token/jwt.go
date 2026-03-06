package token

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/AndrewBudd/boxcutter/vmid/internal/registry"
	"github.com/golang-jwt/jwt/v5"
)

type JWTIssuer struct {
	mu      sync.RWMutex
	key     *ecdsa.PrivateKey
	keyID   string
	ttl     time.Duration
}

func NewJWTIssuer(keyPath string, ttl time.Duration) (*JWTIssuer, error) {
	var key *ecdsa.PrivateKey

	if keyPath != "" {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("reading JWT key: %w", err)
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("no PEM block found in %s", keyPath)
		}
		parsed, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing EC key: %w", err)
		}
		key = parsed
	} else {
		// Generate a new key if none provided
		var err error
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generating EC key: %w", err)
		}
	}

	// Derive key ID from public key
	pubBytes, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	keyID := base64.RawURLEncoding.EncodeToString(pubBytes[:8])

	return &JWTIssuer{
		key:   key,
		keyID: keyID,
		ttl:   ttl,
	}, nil
}

type VMClaims struct {
	ID     string            `json:"id"`
	IP     string            `json:"ip"`
	Labels map[string]string `json:"labels,omitempty"`
}

func (j *JWTIssuer) Mint(rec *registry.VMRecord, audience string) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(j.ttl)

	aud := jwt.ClaimStrings{"*"}
	if audience != "" {
		aud = jwt.ClaimStrings{audience}
	}

	claims := jwt.MapClaims{
		"iss": "vmid.internal",
		"sub": rec.VMID,
		"aud": aud,
		"iat": now.Unix(),
		"exp": exp.Unix(),
		"jti": generateJTI(),
		"vm": VMClaims{
			ID:     rec.VMID,
			IP:     rec.IP,
			Labels: rec.Labels,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = j.keyID

	signed, err := token.SignedString(j.key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("signing JWT: %w", err)
	}

	return signed, exp, nil
}

func (j *JWTIssuer) JWKS() map[string]interface{} {
	j.mu.RLock()
	defer j.mu.RUnlock()

	pub := j.key.PublicKey
	return map[string]interface{}{
		"keys": []map[string]interface{}{
			{
				"kty": "EC",
				"crv": "P-256",
				"use": "sig",
				"kid": j.keyID,
				"alg": "ES256",
				"x":   base64.RawURLEncoding.EncodeToString(pub.X.Bytes()),
				"y":   base64.RawURLEncoding.EncodeToString(pub.Y.Bytes()),
			},
		},
	}
}

func generateJTI() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Pad coordinate to 32 bytes for P-256
func init() {
	_ = big.NewInt(0) // ensure big is imported
}
