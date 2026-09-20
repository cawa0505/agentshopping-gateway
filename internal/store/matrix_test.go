// 7.2 — test-matrix gap coverage. The other 13 matrix scenarios have direct
// tests elsewhere (see store_test.go / service_test.go / policy_test.go);
// this file covers the four that did not: allowance insufficient, monthly
// limit, merchant scope, and currency mismatch at the service boundary.
package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/purchasing"
)

func TestMatrixAllowanceInsufficient(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, func(_ *purchasing.AuthBinding, p *purchasing.AuthorizationPolicy) {
		p.PerOrder = 0 // lift per-order cap so allowance is the binding constraint
		p.Daily = 0    // lift daily cap too (default fixture has 3000)
	})
	now := time.Now().UTC()

	if _, err := reserveIn(t, s, b, 4900, "idem-al1", "agent-a", now); err != nil {
		t.Fatalf("reserve 4900: %v", err)
	}
	// Only 100 left; 200 must be denied and leave no side effects.
	_, err := reserveIn(t, s, b, 200, "idem-al2", "agent-a", now)
	if !errors.Is(err, purchasing.ErrAllowanceExceeded) {
		t.Fatalf("expected allowance exceeded, got %v", err)
	}
	snap, _ := s.Allowance(context.Background(), b.ID, now)
	if snap.Available != 100 || snap.ActiveReserves != 4900 {
		t.Fatalf("allowance after denial = %+v", snap)
	}
}

func TestMatrixMonthlyLimit(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, func(_ *purchasing.AuthBinding, p *purchasing.AuthorizationPolicy) {
		p.Daily = 0    // isolate the monthly cap
		p.Monthly = 1000
	})
	now := time.Now().UTC()

	if _, err := reserveIn(t, s, b, 600, "idem-m1", "agent-a", now); err != nil {
		t.Fatalf("reserve 600: %v", err)
	}
	_, err := reserveIn(t, s, b, 500, "idem-m2", "agent-a", now)
	if !errors.Is(err, purchasing.ErrMonthlyLimitExceeded) {
		t.Fatalf("expected monthly limit, got %v", err)
	}
}

func TestMatrixMerchantScope(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, func(_ *purchasing.AuthBinding, p *purchasing.AuthorizationPolicy) {
		p.ScopeMerchants = []string{"other-shop"}
	})
	now := time.Now().UTC()

	_, err := s.Reserve(context.Background(), ReserveInput{
		BindingID: b.ID, QuoteID: "qt", IdempotencyKey: "idem-mer", AgentID: "agent-a",
		Currency: b.Currency, Amount: 100, RequestedAt: now,
		MerchantID: b.MerchantID, Categories: []string{"books"},
	})
	if !errors.Is(err, purchasing.ErrMerchantNotAllowed) {
		t.Fatalf("expected merchant not allowed, got %v", err)
	}
}

func TestPurchaseCurrencyMismatchRejected(t *testing.T) {
	now := time.Now().UTC()
	adapter := &fakeMerchantAdapter{
		quote: &purchasing.PurchaseQuote{
			QuoteID: "qt_1", MerchantID: "wc-shop-1", Total: 799, Currency: "USD",
			Categories: []string{"books"}, ExpiresAt: now.Add(time.Minute),
		},
	}
	svc, b := testService(t, adapter, now)

	out, err := svc.Purchase(context.Background(), PurchaseRequest{
		BindingID: b.ID, AgentID: "agent-a", MerchantID: "wc-shop-1",
		Items:      []purchasing.PurchaseItem{{ProductID: "p-1", Quantity: 1}},
		Currency:   "TWD", Amount: 799, IdempotencyKey: "idem-cur",
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if out.Status != purchasing.PurchaseRejected || out.Error != "CURRENCY_NOT_SUPPORTED" {
		t.Fatalf("outcome = %+v", out)
	}
	// Rejected before any order side effect.
	if len(adapter.calls) != 1 || adapter.calls[0] != "quote" {
		t.Fatalf("calls = %v, want quote only", adapter.calls)
	}
}
