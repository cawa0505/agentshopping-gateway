package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/nexusledger"
)

// fakeLedger is an in-memory NexusLedger ledger honoring earn/spend/balance
// with atomic 409-on-insufficient semantics.
type fakeLedger struct {
	mu      sync.Mutex
	balance int64
}

func (f *fakeLedger) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ledger/accounts/{id}/earn", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Amount int64 `json:"amount"` }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.balance += req.Amount
		bal := f.balance
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"action_type": "EARN", "balance_after": bal})
	})
	mux.HandleFunc("/v1/ledger/accounts/{id}/spend", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Amount int64 `json:"amount"` }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if req.Amount > f.balance {
			http.Error(w, "insufficient", http.StatusConflict)
			return
		}
		f.balance -= req.Amount
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"action_type": "SPEND", "balance_after": f.balance})
	})
	mux.HandleFunc("/v1/ledger/accounts/{id}/balance", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		bal := f.balance
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"balance": bal})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newService(url string) *Service {
	return New(nexusledger.New(nexusledger.Config{BaseURL: url, SiteID: "test", JWKSTTL: time.Second}))
}

// 4.1 — balance query reports the ledger balance.
func TestBalance(t *testing.T) {
	f := &fakeLedger{balance: 250}
	s := newService(f.server(t).URL)
	bal, err := s.Balance(context.Background(), "acc-1")
	if err != nil || bal != 250 {
		t.Fatalf("balance: got %d err %v, want 250", bal, err)
	}
}

// 4.2 — sufficient points redeem to a discount; insufficient is rejected and
// applies no discount.
func TestRedeem(t *testing.T) {
	f := &fakeLedger{balance: 100}
	s := newService(f.server(t).URL)

	discount, remaining, err := s.Redeem(context.Background(), "acc-1", 40, "order-1")
	if err != nil || discount != 40 || remaining != 60 {
		t.Fatalf("redeem 40: discount=%d remaining=%d err=%v, want 40/60/nil", discount, remaining, err)
	}

	_, _, err = s.Redeem(context.Background(), "acc-1", 1000, "order-2")
	if err != nexusledger.ErrInsufficientBalance {
		t.Fatalf("over-redeem: got err %v, want ErrInsufficientBalance", err)
	}
	// Balance unchanged by the rejected spend.
	if bal, _ := s.Balance(context.Background(), "acc-1"); bal != 60 {
		t.Fatalf("balance after rejected redeem: got %d, want 60", bal)
	}
}

// 4.3 — successful order earns commission; failed order does not.
func TestSettleOrder(t *testing.T) {
	f := &fakeLedger{balance: 0}
	s := newService(f.server(t).URL)

	bal, err := s.SettleOrder(context.Background(), "acc-1", 15, "order-ok", true)
	if err != nil || bal != 15 {
		t.Fatalf("success settle: got %d err %v, want 15", bal, err)
	}

	bal, err = s.SettleOrder(context.Background(), "acc-1", 15, "order-fail", false)
	if err != nil || bal != 15 {
		t.Fatalf("failed settle must not earn: got %d err %v, want 15", bal, err)
	}
}
