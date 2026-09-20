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

// fakeNXL is an in-process NexusLedger: JWKS + ability-check + ledger.
type fakeNXL struct {
	priv    ed25519.PrivateKey
	pub     ed25519.PublicKey
	balance int64
}

func (f *fakeNXL) sign(t *testing.T) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"sub": "agent-1", "exp": time.Now().Add(time.Hour).Unix(),
		"abilities": []string{"cart:write", "checkout:pay", "post-order:read"},
	}).SignedString(f.priv)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *fakeNXL) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/entitlements/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "OKP", "crv": "Ed25519", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(f.pub),
		}}})
	})
	mux.HandleFunc("/v1/abilities/check", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Owned    []string `json:"owned"`
			Required string   `json:"required"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": contains(req.Owned, req.Required)})
	})
	mux.HandleFunc("/v1/ledger/accounts/{id}/spend", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct{ Amount int64 `json:"amount"` }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Amount > f.balance {
			http.Error(w, "insufficient", http.StatusConflict)
			return
		}
		f.balance -= req.Amount
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"balance_after": f.balance})
	})
	mux.HandleFunc("/v1/ledger/accounts/{id}/earn", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct{ Amount int64 `json:"amount"` }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.balance += req.Amount
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"balance_after": f.balance})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeBridge is an in-process store bridge.
func fakeBridge(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /catalog/search", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"products": []map[string]any{
			{"id": 1, "name": "咖啡豆", "sku": "C-1", "price": 350.0, "purchasable": true},
		}})
	})
	mux.HandleFunc("POST /cart/add", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cart":     []map[string]any{{"product_id": 1, "name": "咖啡豆", "qty": 2, "unit_price": 350.0}},
			"subtotal": 700.0, "ok": true,
		})
	})
	mux.HandleFunc("POST /checkout", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"url": "https://store.example/checkout?agentshopping_cart=1:2", "ok": true})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

// 5.1 — full flow: anonymous search → cart → checkout(redeem) → post-order settle.
func TestEndToEndFlow(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	nxlSrv := &fakeNXL{priv: priv, pub: pub, balance: 500}
	nxURL := nxlSrv.server(t).URL
	brURL := fakeBridge(t).URL

	nxl := nexusledger.New(nexusledger.Config{BaseURL: nxURL, SiteID: "test", JWKSTTL: time.Second})
	deps := handlers.Deps{
		Bridge:  bridge.New(bridge.Config{BaseURL: brURL}),
		Pricing: pricing.New(nxl),
	}
	gw := httptest.NewServer(New(nxl, deps))
	t.Cleanup(gw.Close)

	token := nxlSrv.sign(t)

	// 1. Anonymous catalog search (no token).
	resp, out := post(t, gw.URL+"/api/mcp/catalog", "", map[string]any{"keyword": "咖啡"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog: got %d, want 200", resp.StatusCode)
	}
	if products, _ := out["products"].([]any); len(products) != 1 {
		t.Fatalf("catalog: want 1 product, got %v", out["products"])
	}

	// 2. Add to cart (needs cart:write).
	resp, out = post(t, gw.URL+"/api/mcp/cart", token, map[string]any{"product_id": 1, "qty": 2})
	if resp.StatusCode != http.StatusOK || out["ok"] != true {
		t.Fatalf("cart: got %d ok=%v", resp.StatusCode, out["ok"])
	}

	// 3. Checkout with 100-point redemption (needs checkout:pay).
	resp, out = post(t, gw.URL+"/api/mcp/checkout", token, map[string]any{
		"account": "agent-1", "redeem_points": 100, "order_ref": "o-1",
		"cart": []map[string]any{{"product_id": 1, "qty": 2}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkout: got %d", resp.StatusCode)
	}
	if out["discount"].(float64) != 100 {
		t.Fatalf("checkout discount: want 100, got %v", out["discount"])
	}
	if _, ok := out["url"].(string); !ok {
		t.Fatalf("checkout: missing url")
	}

	// 4. Post-order commission on success (needs post-order:read).
	resp, out = post(t, gw.URL+"/api/mcp/post-order", token, map[string]any{
		"account": "agent-1", "amount": 35, "order_ref": "o-1", "success": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-order: got %d", resp.StatusCode)
	}
	// balance: 500 - 100 (redeem) + 35 (commission) = 435.
	if out["balance"].(float64) != 435 {
		t.Fatalf("post-order balance: want 435, got %v", out["balance"])
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
