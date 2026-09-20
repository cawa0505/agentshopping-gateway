package nexusledger

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestRealNXLSmoke exercises the client against a live nxld instance. It is
// skipped unless NXL_SMOKE_URL is set, so normal `go test` stays offline.
//
//	/tmp/nxld &  # NXL_BIND_ADDR=127.0.0.1:18080 ...
//	NXL_SMOKE_URL=http://127.0.0.1:18080 go test ./internal/nexusledger/ -run Smoke -v
func TestRealNXLSmoke(t *testing.T) {
	base := os.Getenv("NXL_SMOKE_URL")
	if base == "" {
		t.Skip("NXL_SMOKE_URL not set; skipping live NexusLedger smoke")
	}

	c := New(Config{BaseURL: base, JWKSTTL: time.Minute})
	ctx := context.Background()

	email := "smoke+" + time.Now().Format("150405.000") + "@test.local"
	// Register an identity, then log in to obtain a real bearer token.
	postJSONRaw(t, base, "/v1/identities", map[string]any{
		"email": email, "password": "secret123", "ref": "agentshop",
	})
	login := postJSONRaw(t, base, "/v1/auth/login", map[string]any{
		"email": email, "password": "secret123",
	})
	token, _ := login["token"].(string)
	if token == "" {
		t.Fatalf("login returned no token: %v", login)
	}

	// 2.1 — JWKS verify yields the abilities claim.
	claims, err := c.VerifyIdentity(ctx, token)
	if err != nil {
		t.Fatalf("VerifyIdentity: %v", err)
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		t.Fatalf("no sub claim in %v", claims)
	}

	// 2.2 — wildcard grant ("*") satisfies a concrete ability.
	allowed, err := c.CheckAbility(ctx, []string{"*"}, "cart:write")
	if err != nil || !allowed {
		t.Fatalf("CheckAbility: allowed=%v err=%v", allowed, err)
	}

	// 4.1/4.2/4.3 — balance, earn, spend, and 409 on overspend.
	if bal, err := c.Balance(ctx, token, sub); err != nil || bal != 0 {
		t.Fatalf("initial balance: got %d err %v, want 0", bal, err)
	}
	if bal, err := c.Earn(ctx, token, sub, 500, "seed"); err != nil || bal != 500 {
		t.Fatalf("earn: got %d err %v, want 500", bal, err)
	}
	if bal, err := c.Spend(ctx, token, sub, 120, "order-1"); err != nil || bal != 380 {
		t.Fatalf("spend: got %d err %v, want 380", bal, err)
	}
	if _, err := c.Spend(ctx, token, sub, 9999, "order-over"); err != ErrInsufficientBalance {
		t.Fatalf("overspend: got err %v, want ErrInsufficientBalance", err)
	}
	// spend must not have moved the balance.
	if bal, _ := c.Balance(ctx, token, sub); bal != 380 {
		t.Fatalf("balance after rejected spend: got %d, want 380", bal)
	}
}

// postJSONRaw is a test-only helper for the public registration/login routes.
func postJSONRaw(t *testing.T, base, path string, payload map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Site-ID", "shop1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: decode: %v", path, err)
	}
	if resp.StatusCode >= 400 {
		t.Fatalf("%s: http %d: %v", path, resp.StatusCode, out)
	}
	return out
}
