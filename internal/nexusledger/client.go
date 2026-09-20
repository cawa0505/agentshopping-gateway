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
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInsufficientBalance is returned by Spend when NexusLedger rejects the
// debit for lack of points (HTTP 409).
var ErrInsufficientBalance = errors.New("insufficient balance")

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
	return c.ledgerURL(id, "balance")
}

func (c Config) ledgerURL(id, action string) string {
	return c.BaseURL + "/v1/ledger/accounts/" + id + "/" + action
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

	body, err := c.httpGet(ctx, "", c.cfg.jwksURL())
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

// CheckAbility reports whether the owned abilities (from the agent's verified
// JWT `abilities` claim) grant the required ability. NexusLedger evaluates
// wildcard/role expansion via HasAbility(owned, required).
func (c *Client) CheckAbility(ctx context.Context, owned []string, required string) (bool, error) {
	payload := map[string]any{"owned": owned, "required": required}
	body, err := c.httpPostJSON(ctx, "", c.cfg.abilitiesURL(), payload)
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

// Balance returns the integer point balance for an account. The ledger routes
// are bearer-protected, so the agent's token must be supplied.
func (c *Client) Balance(ctx context.Context, token, id string) (int64, error) {
	body, err := c.httpGet(ctx, token, c.cfg.balanceURL(id))
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

// Earn credits points to an account (e.g. transaction commission). Returns the
// post-entry account balance.
func (c *Client) Earn(ctx context.Context, token, id string, amount int64, ref string) (int64, error) {
	return c.ledgerWrite(ctx, token, c.cfg.ledgerURL(id, "earn"), amount, ref)
}

// Spend debits points from an account (e.g. point redemption / discount).
// Returns ErrInsufficientBalance when NexusLedger rejects with 409.
func (c *Client) Spend(ctx context.Context, token, id string, amount int64, ref string) (int64, error) {
	return c.ledgerWrite(ctx, token, c.cfg.ledgerURL(id, "spend"), amount, ref)
}

func (c *Client) ledgerWrite(ctx context.Context, token, url string, amount int64, ref string) (int64, error) {
	payload := map[string]any{"amount": amount}
	if ref != "" {
		payload["reference_id"] = ref
	}
	body, status, err := c.postJSON(ctx, token, url, payload)
	if err != nil {
		return 0, err
	}
	if status == http.StatusConflict {
		return 0, ErrInsufficientBalance
	}
	if status >= 400 {
		return 0, fmt.Errorf("ledger write: http %d", status)
	}
	var e struct {
		BalanceAfter int64 `json:"balance_after"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return 0, fmt.Errorf("parse ledger entry: %w", err)
	}
	return e.BalanceAfter, nil
}

// --- HTTP helpers ---

// setHeaders always sends X-Site-ID (NexusLedger's /v1 scope middleware rejects
// requests it cannot resolve to a site) and attaches a bearer token when given.
func (c *Client) setHeaders(req *http.Request, token string) {
	site := c.cfg.SiteID
	if site == "" {
		site = "default"
	}
	req.Header.Set("X-Site-ID", site)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func (c *Client) httpGet(ctx context.Context, token, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, token)
	body, status, err := c.send(req)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("http %d", status)
	}
	return body, nil
}

// httpPostJSON posts and treats any >=400 as an error (ability/generic use).
func (c *Client) httpPostJSON(ctx context.Context, token, url string, payload any) ([]byte, error) {
	body, status, err := c.postJSON(ctx, token, url, payload)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("http %d", status)
	}
	return body, nil
}

// postJSON posts and returns the raw status so callers can branch on it
// (e.g. ledger spend 409). It does not treat >=400 as a transport error.
func (c *Client) postJSON(ctx context.Context, token, url string, payload any) ([]byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setHeaders(req, token)
	return c.send(req)
}

func (c *Client) send(req *http.Request) ([]byte, int, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
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
