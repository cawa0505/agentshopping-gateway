package auth

import (
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

// fakeNXL is a stand-in NexusLedger with a signing key and an ability table.
type fakeNXL struct {
	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	abilities map[string]bool
	down      bool // when true, every endpoint 500s (simulated outage)
}

func newFakeNXL(t *testing.T) *fakeNXL {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeNXL{priv: priv, pub: pub, abilities: map[string]bool{"cart:write": true}}
}

func (f *fakeNXL) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(f.priv)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *fakeNXL) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if f.down {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/v1/entitlements/jwks":
			key := map[string]any{"kty": "OKP", "crv": "Ed25519", "kid": "k1",
				"x": base64.RawURLEncoding.EncodeToString(f.pub)}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{key}})
		case "/v1/abilities/check":
			var req struct {
				Required string `json:"required"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(map[string]any{"allowed": f.abilities[req.Required]})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func client(url string) *nexusledger.Client {
	return nexusledger.New(nexusledger.Config{BaseURL: url, JWKSTTL: time.Second})
}

func ok200(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func do(t *testing.T, h http.Handler, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/x", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// 2.1 — expired / forged JWT on a protected route returns 401.
func TestExpiredTokenRejected(t *testing.T) {
	f := newFakeNXL(t)
	c := client(f.server(t).URL)
	h := Middleware(c, Requirement{Ability: "cart:write"}, http.HandlerFunc(ok200))

	expired := f.sign(t, jwt.MapClaims{"sub": "a1", "exp": time.Now().Add(-time.Hour).Unix()})
	if code := do(t, h, expired); code != http.StatusUnauthorized {
		t.Fatalf("expired token: got %d, want 401", code)
	}
	if code := do(t, h, "garbage.forged.token"); code != http.StatusUnauthorized {
		t.Fatalf("forged token: got %d, want 401", code)
	}
	if code := do(t, h, ""); code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d, want 401", code)
	}
}

// 2.2 — ability gate: lacking ability 403, holding ability passes.
func TestAbilityGate(t *testing.T) {
	f := newFakeNXL(t)
	c := client(f.server(t).URL)

	valid := f.sign(t, jwt.MapClaims{"sub": "a1", "exp": time.Now().Add(time.Hour).Unix()})

	deny := Middleware(c, Requirement{Ability: "checkout:pay"}, http.HandlerFunc(ok200))
	if code := do(t, deny, valid); code != http.StatusForbidden {
		t.Fatalf("missing ability: got %d, want 403", code)
	}

	allow := Middleware(c, Requirement{Ability: "cart:write"}, http.HandlerFunc(ok200))
	if code := do(t, allow, valid); code != http.StatusOK {
		t.Fatalf("held ability: got %d, want 200", code)
	}
}

// 2.3 — read-only browsing degrades open when NexusLedger is down.
func TestReadOnlyDegradesOpen(t *testing.T) {
	f := newFakeNXL(t)
	f.down = true
	c := client(f.server(t).URL)
	h := Middleware(c, Requirement{ReadOnly: true}, http.HandlerFunc(ok200))

	if code := do(t, h, ""); code != http.StatusOK {
		t.Fatalf("anonymous browse during outage: got %d, want 200", code)
	}
	// A present-but-unverifiable token must not block browsing either.
	if code := do(t, h, "stale.token.here"); code != http.StatusOK {
		t.Fatalf("degraded browse with stale token: got %d, want 200", code)
	}
}
