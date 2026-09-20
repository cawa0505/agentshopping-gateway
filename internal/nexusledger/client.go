// Package nexusledger is a thin client for the parts of NexusLedger the
// gateway depends on: JWKS retrieval (with a short-lived cache), ability
// authorization, point-balance queries, and Ed25519 JWT verification.
//
// The gateway never signs JWTs itself; it consumes NexusLedger-issued Ed25519
// tokens and defers ledger earn/spend to NexusLedger's atomic endpoints.
package nexusledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Config pins the NexusLedger endpoints and defaults the tenant.
type Config struct {
	BaseURL   string        // e.g. http://localhost:8080
	JWKSURL   string        // default: {BaseURL}/v1/entitlements/jwks
	Abilities string        // default: {BaseURL}/v1/abilities/check
	Balance   string        // default: {BaseURL}/v1/ledger/accounts/{id}/balance
	SiteID    string        // default: "" (NexusLedger falls back to "default")
	JWKSTTL   time.Duration // cache lifetime for JWKS
}

// JWKS is a subset of the RFC 7517 keyset we need (Ed25519/OKP).
type JWKS struct {
	Keys []struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		Kid string `json:"kid"`
		X   string `json:"x"`
	} `json:"keys"`
}

func (c Config) jwksURL() string {
	if c.JWKSURL != "" {
		return c.JWKSURL
	}
	return c.BaseURL + "/v1/entitlements/jwks"
}

func (c Config) abilitiesURL() string {
	if c.Abilities != "" {
		return c.Abilities
	}
	return c.BaseURL + "/v1/abilities/check"
}

func (c Config) balanceURL(id string) string {
	if c.Balance != "" {
		return c.Balance
	}
	return c.BaseURL + "/v1/ledger/accounts/" + id + "/balance"
}

// Client talks to NexusLedger.
type Client struct {
	cfg        Config
	httpClient *http.Client
	mu         sync.Mutex
	keys       *JWKS
	keysAt     time.Time
}

// New wires a Client with the given config, using http.DefaultClient.
func New(cfg Config) *Client {
	return &Client{cfg: cfg, httpClient: http.DefaultClient}
}

// FetchJWKS refreshes and returns the keys. Errors are returned only when the
// fetch fails outright; a stale cached copy is returned on partial failure so
// auth degrades to "verify against last known good key" rather than hard fail.
func (c *Client) FetchJWKS(ctx context.Context) (*JWKS, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.keys != nil && time.Since(c.keysAt) < c.cfg.JWKSTTL {
		return c.keys, nil
	}

	body, err := c.httpGet(ctx, c.cfg.jwksURL())
	if err != nil {
		if c.keys == nil {
			return nil, fmt.Errorf("fetch jwks: %w", err)
		}
		return c.keys, nil
	}

	var jwks JWKS
	if err := json.Unmarshal(body, &jwks); err != nil {
		if c.keys == nil {
			return nil, fmt.Errorf("parse jwks: %w", err)
		}
		return c.keys, nil
	}

	// Guard: require at least one Ed25519 key.
	if len(jwks.Keys) == 0 {
		return nil, fmt.Errorf("jwks contains no keys")
	}
	c.keys = &jwks
	c.keysAt = time.Now()
	return &jwks, nil
}

// CheckAbility reports whether the agent owns a wildcard/role that grants the
// required ability.
func (c *Client) CheckAbility(ctx context.Context, agent, required string) (bool, error) {
	payload := map[string]any{"owned": []string{agent}, "required": required}
	body, err := c.httpPostJSON(ctx, c.cfg.abilitiesURL(), payload)
	if err != nil {
		return false, fmt.Errorf("ability check %q: %w", required, err)
	}
	var res struct {
		Allowed bool `json:"allowed"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return false, fmt.Errorf("parse ability response: %w", err)
	}
	return res.Allowed, nil
}

// Balance returns the integer point balance for an account.
func (c *Client) Balance(ctx context.Context, id string) (int64, error) {
	body, err := c.httpGet(ctx, c.cfg.balanceURL(id))
	if err != nil {
		return 0, fmt.Errorf("balance %s: %w", id, err)
	}
	var res struct {
		Balance int64 `json:"balance"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return 0, fmt.Errorf("parse balance: %w", err)
	}
	return res.Balance, nil
}

// VerifyIdentity validates a NexusLedger JWT against the JWKS and returns the
// embedded claims. Returns an error if the token is missing, expired, or not
// signed by a trusted Ed25519 key.
func (c *Client) VerifyIdentity(ctx context.Context, token string) (map[string]any, error) {
	keys, err := c.FetchJWKS(ctx)
	if err != nil {
		return nil, err
	}
	for _, key := range keys.Keys {
		pub, err := decodeEd25519PublicKey(key.X)
		if err != nil {
			continue
		}
		claims := jwt.MapClaims{}
		_, err = jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
				return nil, fmt.Errorf("unexpected signing method %q", t.Method.Alg())
			}
			return pub, nil
		})
		if err == nil {
			return claims, nil
		}
	}
	return nil, fmt.Errorf("jwt invalid (expired or bad signature)")
}

// --- HTTP helpers ---

func (c *Client) httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Client) httpPostJSON(ctx context.Context, url string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func decodeEd25519PublicKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad key size")
	}
	return ed25519.PublicKey(raw), nil
}
