package auth

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

	"github.com/cawa0505/agentshopping-gateway/internal/nexusledger"
	"github.com/golang-jwt/jwt/v5"
)

// benchNXL supplies a JWKS and a trivial ability endpoint so the middleware
// benchmark can run end to end. The interesting cost is local Ed25519 verify.
func benchNXL(b *testing.B) (*nexusledger.Client, ed25519.PrivateKey, string) {
	b.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/entitlements/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "OKP", "crv": "Ed25519", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(pub),
		}}})
	})
	mux.HandleFunc("/v1/abilities/check", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true})
	})
	srv := httptest.NewServer(mux)
	b.Cleanup(srv.Close)

	c := nexusledger.New(nexusledger.Config{BaseURL: srv.URL, JWKSTTL: time.Hour})
	if _, err := c.FetchJWKS(context.Background()); err != nil {
		b.Fatal(err)
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"sub": "agent-1", "exp": time.Now().Add(time.Hour).Unix(),
		"abilities": []string{"cart:write"},
	}).SignedString(priv)
	if err != nil {
		b.Fatal(err)
	}
	return c, priv, tok
}

// BenchmarkJWTVerify — pure Ed25519 verify of a NexusLedger token against a
// cached JWKS. No network: this is the CPU cost the gateway pays per request.
func BenchmarkJWTVerify(b *testing.B) {
	c, _, tok := benchNXL(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := c.VerifyIdentity(context.Background(), tok); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMiddleware — the full auth middleware (verify + ability check).
// Network to the fake NXL is loopback, so the delta vs BenchmarkJWTVerify is
// the ability-check round trip.
func BenchmarkMiddleware(b *testing.B) {
	c, _, tok := benchNXL(b)
	h := Middleware(c, Requirement{Ability: "cart:write"}, http.HandlerFunc(ok200))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/mcp/x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status %d", rec.Code)
		}
	}
}
