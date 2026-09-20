package nexusledger

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testServer stands in for the NexusLedger entitlement endpoints so JWKS
// caching, JWT verification, and ability checks can run offline.
type testServer struct {
	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	abilities map[string]bool
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testServer{
		priv:      priv,
		pub:       pub,
		abilities: map[string]bool{"catalog:read": true, "cart:write": true, "checkout:pay": false},
	}
}

func (s *testServer) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(s.priv)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (s *testServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/entitlements/jwks":
			key := map[string]any{
				"kty": "OKP", "crv": "Ed25519", "kid": "agent-key-1",
				"x": base64.RawURLEncoding.EncodeToString(s.pub),
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{key}})
		case "/v1/abilities/check":
			var req struct {
				Required string `json:"required"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"allowed": s.abilities[req.Required]})
		case "/v1/ledger/accounts/any/balance":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"balance": int64(100)})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	return mux
}

func newClient(url string) *Client {
	return New(Config{
		BaseURL:   url,
		SiteID:    "test",
		JWKSURL:   url + "/v1/entitlements/jwks",
		Abilities: url + "/v1/abilities/check",
		Balance:   url + "/v1/ledger/accounts/any/balance",
		JWKSTTL:   5 * time.Second,
	})
}

func TestVerifyValidToken(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	client := newClient(srv.URL)

	token := s.sign(t, jwt.MapClaims{"sub": "agent-1", "exp": time.Now().Add(time.Hour).Unix()})
	claims, err := client.VerifyIdentity(context.Background(), token)
	if err != nil {
		t.Fatalf("expected valid token: %v", err)
	}
	if claims["sub"] != "agent-1" {
		t.Fatalf("expected sub=agent-1, got %v", claims["sub"])
	}
}

func TestVerifyExpiredToken(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	client := newClient(srv.URL)

	token := s.sign(t, jwt.MapClaims{"sub": "agent-1", "exp": time.Now().Add(-time.Hour).Unix()})
	if _, err := client.VerifyIdentity(context.Background(), token); err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestCheckAbility(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	client := newClient(srv.URL)

	allowed, err := client.CheckAbility(context.Background(), []string{"catalog:read", "cart:write"}, "catalog:read")
	if err != nil || !allowed {
		t.Fatalf("expected catalog:read allowed: %v %v", allowed, err)
	}
	denied, err := client.CheckAbility(context.Background(), []string{"catalog:read"}, "checkout:pay")
	if err != nil || denied {
		t.Fatalf("expected checkout:pay denied: %v %v", denied, err)
	}
}

func TestBalance(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	client := newClient(srv.URL)

	bal, err := client.Balance(context.Background(), "tok", "any")
	if err != nil || bal != 100 {
		t.Fatalf("expected balance 100: %v %v", bal, err)
	}
}
