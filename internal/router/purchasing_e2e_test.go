// 7.1 — in-process E2E for the delegated purchasing module: real gateway
// handler stack (auth middleware → purchasing handler → service → SQLite
// store) against a fake NXL and a fake bridge, over loopback HTTP only.
package router

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/bridge"
	"github.com/cawa0505/agentshopping-gateway/internal/handlers"
	"github.com/cawa0505/agentshopping-gateway/internal/nexusledger"
	"github.com/cawa0505/agentshopping-gateway/internal/pricing"
	"github.com/cawa0505/agentshopping-gateway/internal/purchasing"
	"github.com/cawa0505/agentshopping-gateway/internal/store"
	"github.com/golang-jwt/jwt/v5"
)

// TestPurchasingEndToEnd covers: no-ability → 403, quote-first purchase →
// capture, allowance reflects the spend, idempotent replay returns the same
// result without double-spending, and a foreign agent is denied (7.2).
func TestPurchasingEndToEnd(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	nxlSrv := &fakeNXL{priv: priv, pub: pub, balance: 500}
	nxURL := nxlSrv.server(t).URL

	// Fake bridge with the autonomous-order endpoints.
	bridgeMux := http.NewServeMux()
	bridgeMux.HandleFunc("POST /orders/quote", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"quote_id": "qt_1", "items": []map[string]any{{"product_id": 1, "qty": 2}},
			"total": 700.0, "currency": "TWD", "categories": []string{"food"},
			"expires_at": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
		})
	})
	bridgeMux.HandleFunc("POST /orders", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"order_id": 100, "status": "pending", "total": 700.0, "currency": "TWD",
		})
	})
	bridgeMux.HandleFunc("GET /orders/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "processing"})
	})
	brSrv := httptest.NewServer(bridgeMux)
	t.Cleanup(brSrv.Close)

	st, err := store.New(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	adapter := bridge.NewWooCommerceAdapter(bridge.New(bridge.Config{BaseURL: brSrv.URL, APIKey: "k", StoreID: "shop1"}))
	svc := store.NewService(st, adapter, time.Now)

	// Binding delegated to agent-1 with a 5000 TWD allowance, category food.
	now := time.Now().UTC()
	b := &purchasing.AuthBinding{
		ID: store.NewID("ab_"), UserID: "usr_1", AgentID: "agent-1",
		PaymentMethodRef: "pm_store_abc", Currency: "TWD",
		Status: purchasing.BindingActive, MerchantID: "shop1", CreatedAt: now,
	}
	pol := &purchasing.AuthorizationPolicy{
		ID: store.NewID("pl_"), BindingID: b.ID, AllowanceAmount: 5000,
		PerOrder: 1500, Daily: 3000, Monthly: 10000,
		ScopeCategories: []string{"food"}, ApprovalMode: "auto", CreatedAt: now,
	}
	if err := st.CreateBinding(context.Background(), b, pol); err != nil {
		t.Fatal(err)
	}

	nxl := nexusledger.New(nexusledger.Config{BaseURL: nxURL, SiteID: "test", JWKSTTL: time.Second})
	deps := handlers.Deps{
		Bridge:     bridge.New(bridge.Config{BaseURL: brSrv.URL, APIKey: "k", StoreID: "shop1"}),
		Pricing:    pricing.New(nxl),
		Purchasing: svc,
	}
	gw := httptest.NewServer(New(nxl, deps))
	t.Cleanup(gw.Close)
	token := nxlSrv.sign(t)

	// 1. Token lacks purchasing:pay → 403.
	resp, out := post(t, gw.URL+"/api/mcp/purchasing", token, map[string]any{
		"action": "get_allowance", "binding_id": b.ID,
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("purchasing without ability: got %d, want 403", resp.StatusCode)
	}

	// 2. Token with purchasing:pay → full purchase flow captures.
	tok2 := signWithAbilities(t, nxlSrv, []string{"purchasing:pay"})
	resp, out = post(t, gw.URL+"/api/mcp/purchasing", tok2, map[string]any{
		"action": "purchase", "binding_id": b.ID, "merchant_id": "shop1",
		"items":  []map[string]any{{"product_id": "1", "quantity": 2}},
		"amount": 700, "currency": "TWD", "idempotency_key": "idem-e2e-1",
	})
	if resp.StatusCode != http.StatusOK || out["ok"] != true {
		t.Fatalf("purchase: got %d %v", resp.StatusCode, out)
	}
	if out["status"] != string(purchasing.PurchaseCaptured) {
		t.Fatalf("purchase status = %v, want CAPTURED", out["status"])
	}

	// 3. Allowance reflects the 700 spend.
	allow, err := svc.GetAllowance(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if allow.Available != 4300 {
		t.Fatalf("available = %d, want 4300", allow.Available)
	}

	// 4. Idempotent replay → same result, no double spend.
	resp, out = post(t, gw.URL+"/api/mcp/purchasing", tok2, map[string]any{
		"action": "purchase", "binding_id": b.ID, "merchant_id": "shop1",
		"items":  []map[string]any{{"product_id": "1", "quantity": 2}},
		"amount": 700, "currency": "TWD", "idempotency_key": "idem-e2e-1",
	})
	if resp.StatusCode != http.StatusOK || out["ok"] != true || out["idempotent_replay"] != true {
		t.Fatalf("replay: got %d %v", resp.StatusCode, out)
	}
	allow2, _ := svc.GetAllowance(context.Background(), b.ID)
	if allow2.Available != 4300 {
		t.Fatalf("replay double-spent: available = %d", allow2.Available)
	}

	// 5. Foreign agent denied (spec 7.2).
	tok3 := signWithSubAbilities(t, nxlSrv, "agent-2", []string{"purchasing:pay"})
	resp, out = post(t, gw.URL+"/api/mcp/purchasing", tok3, map[string]any{
		"action": "get_allowance", "binding_id": b.ID,
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign agent: got %d, want 403", resp.StatusCode)
	}
}

// signWithAbilities signs a token for agent-1 with the given abilities.
func signWithAbilities(t *testing.T, f *fakeNXL, abilities []string) string {
	t.Helper()
	return signClaims(t, f, "agent-1", abilities)
}

// signWithSubAbilities signs a token for an arbitrary subject.
func signWithSubAbilities(t *testing.T, f *fakeNXL, sub string, abilities []string) string {
	t.Helper()
	return signClaims(t, f, sub, abilities)
}

func signClaims(t *testing.T, f *fakeNXL, sub string, abilities []string) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"sub": sub, "exp": time.Now().Add(time.Hour).Unix(),
		"abilities": abilities,
	}).SignedString(f.priv)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
