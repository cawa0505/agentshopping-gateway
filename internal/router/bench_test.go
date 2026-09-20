package router

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/bridge"
	"github.com/cawa0505/agentshopping-gateway/internal/handlers"
	"github.com/cawa0505/agentshopping-gateway/internal/nexusledger"
	"github.com/cawa0505/agentshopping-gateway/internal/pricing"
	"github.com/golang-jwt/jwt/v5"
)

// benchEnv is a fully in-process gateway: fake NexusLedger (JWKS + ability +
// ledger) and fake store bridge. It measures the whole hot path an agent hits —
// JWT verify → ability check → module handler → bridge HTTP → ledger HTTP.
type benchEnv struct {
	srv    *httptest.Server
	token  string
	mux    *http.ServeMux
	client *http.Client
}

func newBenchEnv(b *testing.B) *benchEnv {
	b.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}

	nxl := http.NewServeMux()
	nxl.HandleFunc("/v1/entitlements/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "OKP", "crv": "Ed25519", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(pub),
		}}})
	})
	nxl.HandleFunc("/v1/abilities/check", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true})
	})
	nxl.HandleFunc("/v1/ledger/accounts/{id}/spend", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"balance_after": 100})
	})
	nxl.HandleFunc("/v1/ledger/accounts/{id}/earn", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"balance_after": 100})
	})
	nxlSrv := httptest.NewServer(nxl)
	b.Cleanup(nxlSrv.Close)

	store := http.NewServeMux()
	store.HandleFunc("GET /catalog/search", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"products": []map[string]any{
			{"id": 1, "name": "咖啡豆", "sku": "C-1", "price": 350.0, "purchasable": true},
		}})
	})
	store.HandleFunc("POST /cart/add", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cart": []map[string]any{{"product_id": 1, "name": "咖啡豆", "qty": 2, "unit_price": 350.0}},
			"subtotal": 700.0, "ok": true,
		})
	})
	store.HandleFunc("POST /checkout", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"url": "https://store.example/checkout?x=1", "ok": true})
	})
	storeSrv := httptest.NewServer(store)
	b.Cleanup(storeSrv.Close)

	nxlClient := nexusledger.New(nexusledger.Config{BaseURL: nxlSrv.URL, JWKSTTL: time.Hour})
	deps := handlers.Deps{
		Bridge:  bridge.New(bridge.Config{BaseURL: storeSrv.URL}),
		Pricing: pricing.New(nxlClient),
	}
	mux := New(nxlClient, deps)
	gw := httptest.NewServer(mux)
	b.Cleanup(gw.Close)

	tok, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"sub": "agent-1", "exp": time.Now().Add(time.Hour).Unix(),
		"abilities": []string{"cart:write", "checkout:pay", "post-order:read"},
	}).SignedString(priv)
	if err != nil {
		b.Fatal(err)
	}

	// Warm the JWKS cache so the timed loop measures steady-state, not the
	// one-time key fetch.
	_, _ = nxlClient.FetchJWKS(context.Background())

	return &benchEnv{srv: gw, token: tok, mux: mux, client: gw.Client()}
}

func (e *benchEnv) do(b *testing.B, path, body string) {
	b.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		b.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := e.client.Do(req)
	if err != nil {
		b.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("%s: status %d", path, resp.StatusCode)
	}
}

// BenchmarkCatalogSearch — auth (JWT verify) + bridge search.
func BenchmarkCatalogSearch(b *testing.B) {
	e := newBenchEnv(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		e.do(b, "/api/mcp/catalog", `{"keyword":"咖啡"}`)
	}
}

// BenchmarkCartAdd — auth + ability check + bridge cart mutation.
func BenchmarkCartAdd(b *testing.B) {
	e := newBenchEnv(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		e.do(b, "/api/mcp/cart", `{"cart":[],"product_id":1,"qty":2}`)
	}
}

// BenchmarkCheckoutRedeem — auth + ability check + ledger spend + bridge checkout.
// This is the heaviest path: two downstream HTTP calls plus the auth checks.
func BenchmarkCheckoutRedeem(b *testing.B) {
	e := newBenchEnv(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		e.do(b, "/api/mcp/checkout", `{"cart":[{"product_id":1,"qty":1}],"account":"a1","redeem_points":10,"order_ref":"o1"}`)
	}
}
