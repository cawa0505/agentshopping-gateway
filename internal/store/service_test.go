package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/purchasing"
)

type fakeMerchantAdapter struct {
	quote        *purchasing.PurchaseQuote
	quoteErr     error
	order        *purchasing.PurchaseResult
	orderErr     error
	statuses     map[string]string
	statusErr    error
	calls        []string
	createdOrder string
}

func (f *fakeMerchantAdapter) Quote(items []purchasing.PurchaseItem) (*purchasing.PurchaseQuote, error) {
	f.calls = append(f.calls, "quote")
	if f.quoteErr != nil {
		return nil, f.quoteErr
	}
	return f.quote, nil
}

func (f *fakeMerchantAdapter) CreateOrder(quote *purchasing.PurchaseQuote, paymentMethodRef, agentID string) (*purchasing.PurchaseResult, error) {
	f.calls = append(f.calls, "create_order")
	if f.orderErr != nil {
		return nil, f.orderErr
	}
	if f.order != nil && f.order.MerchantOrderRef != "" {
		f.createdOrder = f.order.MerchantOrderRef
	}
	return f.order, nil
}

func (f *fakeMerchantAdapter) GetOrderStatus(orderRef string) (string, error) {
	f.calls = append(f.calls, "get_order_status")
	if f.statusErr != nil {
		return "", f.statusErr
	}
	return f.statuses[orderRef], nil
}

func testService(t *testing.T, adapter *fakeMerchantAdapter, now time.Time) (*Service, *purchasing.AuthBinding) {
	t.Helper()
	s := testStore(t)
	svc := NewService(s, adapter, func() time.Time { return now })
	b := testBinding(t, s, nil)
	return svc, b
}

func TestPurchaseHappyPathCaptures(t *testing.T) {
	now := time.Now().UTC()
	adapter := &fakeMerchantAdapter{
		quote: &purchasing.PurchaseQuote{
			QuoteID: "qt_1", MerchantID: "wc-shop-1", Total: 799, Currency: "TWD",
			Categories: []string{"books"}, ExpiresAt: now.Add(time.Minute),
		},
		order: &purchasing.PurchaseResult{Success: true, MerchantOrderRef: "wc-100"},
		statuses: map[string]string{"wc-100": "processing"},
	}
	svc, b := testService(t, adapter, now)

	out, err := svc.Purchase(context.Background(), PurchaseRequest{
		BindingID: b.ID, AgentID: "agent-a", MerchantID: "wc-shop-1",
		Items: []purchasing.PurchaseItem{{ProductID: "p-1", Quantity: 1}},
		Currency: "TWD", Amount: 799, IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if out.Status != purchasing.PurchaseCaptured {
		t.Fatalf("status = %s, want CAPTURED", out.Status)
	}
	if out.Purchase == nil || out.Purchase.MerchantOrderRef != "wc-100" {
		t.Fatalf("purchase = %+v", out.Purchase)
	}
	snap, _ := svc.GetAllowance(context.Background(), b.ID)
	if snap.Available != 5000-799 || snap.Captured != 799 {
		t.Fatalf("allowance = %+v", snap)
	}
	if len(adapter.calls) != 3 {
		t.Fatalf("calls = %v, want quote/create_order/get_order_status", adapter.calls)
	}
}

func TestPurchaseAccessDeniedForForeignAgent(t *testing.T) {
	now := time.Now().UTC()
	adapter := &fakeMerchantAdapter{
		quote: &purchasing.PurchaseQuote{
			QuoteID: "qt_1", MerchantID: "wc-shop-1", Total: 500, Currency: "TWD",
			Categories: []string{"books"}, ExpiresAt: now.Add(time.Minute),
		},
	}
	s := testStore(t)
	svc := NewService(s, adapter, func() time.Time { return now })
	b := testBinding(t, s, func(b *purchasing.AuthBinding, p *purchasing.AuthorizationPolicy) {
		b.AgentID = "agent-owner"
	})

	_, err := svc.Store.Reserve(context.Background(), ReserveInput{
		BindingID: b.ID, QuoteID: "qt_x", IdempotencyKey: "idem-x",
		AgentID: "agent-other", Currency: "TWD", Amount: 500, RequestedAt: now,
		MerchantID: "wc-shop-1", Categories: []string{"books"},
	})
	if err != purchasing.ErrBindingAccessDenied {
		t.Fatalf("err = %v, want ErrBindingAccessDenied", err)
	}

	// Delegated agent passes the ownership gate.
	res, err := svc.Store.Reserve(context.Background(), ReserveInput{
		BindingID: b.ID, QuoteID: "qt_x", IdempotencyKey: "idem-x",
		AgentID: "agent-owner", Currency: "TWD", Amount: 500, RequestedAt: now,
		MerchantID: "wc-shop-1", Categories: []string{"books"},
	})
	if err != nil || res.IdempotentRe {
		t.Fatalf("owner agent reserve: err=%v idem=%v", err, res.IdempotentRe)
	}
}

func TestReconcileExecutingCapturesAndReleases(t *testing.T) {
	now := time.Now().UTC()
	adapter := &fakeMerchantAdapter{
		quote: &purchasing.PurchaseQuote{
			QuoteID: "qt_1", MerchantID: "wc-shop-1", Total: 500, Currency: "TWD",
			Categories: []string{"books"}, ExpiresAt: now.Add(time.Minute),
		},
		order: &purchasing.PurchaseResult{Success: true, MerchantOrderRef: "wc-100"},
		// Merchant still pending at purchase time → stays EXECUTING.
		statuses: map[string]string{"wc-100": "pending"},
	}
	s := testStore(t)
	svc := NewService(s, adapter, func() time.Time { return now })
	b := testBinding(t, s, nil)

	out, err := svc.Purchase(context.Background(), PurchaseRequest{
		BindingID: b.ID, AgentID: "agent-a", MerchantID: "wc-shop-1",
		Items: []purchasing.PurchaseItem{{ProductID: "p-1", Quantity: 1}},
		Currency: "TWD", Amount: 500, IdempotencyKey: "idem-r1",
	})
	if err != nil || out.Status != purchasing.PurchaseExecuting {
		t.Fatalf("purchase = %+v err=%v", out, err)
	}

	// Merchant flips to processing → reconcile captures.
	adapter.statuses["wc-100"] = "processing"
	settled, err := svc.ReconcileExecuting(context.Background())
	if err != nil || len(settled) != 1 || settled[0].Status != purchasing.PurchaseCaptured {
		t.Fatalf("reconcile capture = %+v err=%v", settled, err)
	}
	snap, _ := svc.GetAllowance(context.Background(), b.ID)
	if snap.Available != 5000-500 || snap.Captured != 500 {
		t.Fatalf("allowance after capture = %+v", snap)
	}

	// A second purchase whose order is pending at purchase time, then fails
	// at the merchant → reconcile releases.
	adapter.order = &purchasing.PurchaseResult{Success: true, MerchantOrderRef: "wc-101"}
	adapter.statuses["wc-101"] = "pending"
	out2, err := svc.Purchase(context.Background(), PurchaseRequest{
		BindingID: b.ID, AgentID: "agent-a", MerchantID: "wc-shop-1",
		Items: []purchasing.PurchaseItem{{ProductID: "p-1", Quantity: 1}},
		Currency: "TWD", Amount: 500, IdempotencyKey: "idem-r2",
	})
	if err != nil || out2.Status != purchasing.PurchaseExecuting {
		t.Fatalf("purchase2 = %+v err=%v", out2, err)
	}
	adapter.statuses["wc-101"] = "failed"
	settled, err = svc.ReconcileExecuting(context.Background())
	if err != nil || len(settled) != 1 || settled[0].Status != purchasing.PurchaseReleased {
		t.Fatalf("reconcile release = %+v err=%v", settled, err)
	}
	snap, _ = svc.GetAllowance(context.Background(), b.ID)
	if snap.Available != 5000-500 || snap.ActiveReserves != 0 {
		t.Fatalf("allowance after release = %+v", snap)
	}
}

func TestPurchaseAmountMismatchRejectsWithoutSideEffects(t *testing.T) {
	now := time.Now().UTC()
	adapter := &fakeMerchantAdapter{
		quote: &purchasing.PurchaseQuote{
			QuoteID: "qt_1", MerchantID: "wc-shop-1", Total: 799, Currency: "TWD",
			Categories: []string{"books"}, ExpiresAt: now.Add(time.Minute),
		},
		order: &purchasing.PurchaseResult{Success: true, MerchantOrderRef: "wc-100"},
		statuses: map[string]string{"wc-100": "processing"},
	}
	svc, b := testService(t, adapter, now)

	out, err := svc.Purchase(context.Background(), PurchaseRequest{
		BindingID: b.ID, AgentID: "agent-a", Items: []purchasing.PurchaseItem{{ProductID: "p-1"}},
		Currency: "TWD", Amount: 800, IdempotencyKey: "idem-2",
	})
	if err != nil || out.Status != purchasing.PurchaseRejected || out.Error != "AMOUNT_MISMATCH" {
		t.Fatalf("outcome = %+v", out)
	}
	snap, _ := svc.GetAllowance(context.Background(), b.ID)
	if snap.Available != 5000 {
		t.Fatalf("allowance changed: %+v", snap)
	}
	if len(adapter.calls) != 1 || adapter.calls[0] != "quote" {
		t.Fatalf("unexpected calls: %v", adapter.calls)
	}
}

func TestPurchaseOrderFailureReleasesReservation(t *testing.T) {
	now := time.Now().UTC()
	adapter := &fakeMerchantAdapter{
		quote: &purchasing.PurchaseQuote{
			QuoteID: "qt_1", MerchantID: "wc-shop-1", Total: 799, Currency: "TWD",
			Categories: []string{"books"}, ExpiresAt: now.Add(time.Minute),
		},
		orderErr: errors.New("payment declined"),
	}
	svc, b := testService(t, adapter, now)

	out, err := svc.Purchase(context.Background(), PurchaseRequest{
		BindingID: b.ID, AgentID: "agent-a", Items: []purchasing.PurchaseItem{{ProductID: "p-1"}},
		Currency: "TWD", Amount: 799, IdempotencyKey: "idem-3",
	})
	if err == nil {
		t.Fatal("expected order error")
	}
	if out.Status != purchasing.PurchaseReleased {
		t.Fatalf("status = %s, want RELEASED", out.Status)
	}
	snap, _ := svc.GetAllowance(context.Background(), b.ID)
	if snap.Available != 5000 || snap.ActiveReserves != 0 {
		t.Fatalf("allowance = %+v", snap)
	}
}

func TestPurchasePendingLeavesReservationActive(t *testing.T) {
	now := time.Now().UTC()
	adapter := &fakeMerchantAdapter{
		quote: &purchasing.PurchaseQuote{
			QuoteID: "qt_1", MerchantID: "wc-shop-1", Total: 799, Currency: "TWD",
			Categories: []string{"books"}, ExpiresAt: now.Add(time.Minute),
		},
		order:    &purchasing.PurchaseResult{Success: true, MerchantOrderRef: "wc-101"},
		statuses: map[string]string{"wc-101": "pending"},
	}
	svc, b := testService(t, adapter, now)

	out, err := svc.Purchase(context.Background(), PurchaseRequest{
		BindingID: b.ID, AgentID: "agent-a", Items: []purchasing.PurchaseItem{{ProductID: "p-1"}},
		Currency: "TWD", Amount: 799, IdempotencyKey: "idem-4",
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if out.Status != purchasing.PurchaseExecuting {
		t.Fatalf("status = %s, want EXECUTING", out.Status)
	}
	snap, _ := svc.GetAllowance(context.Background(), b.ID)
	if snap.ActiveReserves != 799 {
		t.Fatalf("active reserve = %d, want 799", snap.ActiveReserves)
	}
}

func TestPurchaseIdempotentReplayDoesNotReExecute(t *testing.T) {
	now := time.Now().UTC()
	adapter := &fakeMerchantAdapter{
		quote: &purchasing.PurchaseQuote{
			QuoteID: "qt_1", MerchantID: "wc-shop-1", Total: 799, Currency: "TWD",
			Categories: []string{"books"}, ExpiresAt: now.Add(time.Minute),
		},
		order:    &purchasing.PurchaseResult{Success: true, MerchantOrderRef: "wc-102"},
		statuses: map[string]string{"wc-102": "processing"},
	}
	svc, b := testService(t, adapter, now)
	req := PurchaseRequest{
		BindingID: b.ID, AgentID: "agent-a", Items: []purchasing.PurchaseItem{{ProductID: "p-1"}},
		Currency: "TWD", Amount: 799, IdempotencyKey: "idem-5",
	}

	first, err := svc.Purchase(context.Background(), req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := svc.Purchase(context.Background(), req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.IdempotentRe || second.Purchase.ID != first.Purchase.ID {
		t.Fatalf("replay mismatch: %+v vs %+v", second, first)
	}
	if len(adapter.calls) != 4 {
		t.Fatalf("calls = %v, want quote/create_order/get_order_status + quote(replay)", adapter.calls)
	}
}

func TestPurchaseExpiredQuoteRejected(t *testing.T) {
	now := time.Now().UTC()
	adapter := &fakeMerchantAdapter{
		quote: &purchasing.PurchaseQuote{
			QuoteID: "qt_1", MerchantID: "wc-shop-1", Total: 799, Currency: "TWD",
			Categories: []string{"books"}, ExpiresAt: now.Add(-time.Second),
		},
	}
	svc, b := testService(t, adapter, now)

	out, err := svc.Purchase(context.Background(), PurchaseRequest{
		BindingID: b.ID, AgentID: "agent-a", Items: []purchasing.PurchaseItem{{ProductID: "p-1"}},
		Currency: "TWD", Amount: 799, IdempotencyKey: "idem-6",
	})
	if !errors.Is(err, nil) || out.Error != "QUOTE_EXPIRED" {
		t.Fatalf("expected quote expired, got err=%v out=%+v", err, out)
	}
	if out.Status != purchasing.PurchaseRejected {
		t.Fatalf("status = %s, want REJECTED", out.Status)
	}
}

func TestPurchasePolicyRejectDoesNotPlaceOrder(t *testing.T) {
	now := time.Now().UTC()
	adapter := &fakeMerchantAdapter{
		quote: &purchasing.PurchaseQuote{
			QuoteID: "qt_1", MerchantID: "wc-shop-1", Total: 799, Currency: "TWD",
			Categories: []string{"electronics"}, ExpiresAt: now.Add(time.Minute),
		},
		order: &purchasing.PurchaseResult{Success: true, MerchantOrderRef: "wc-103"},
	}
	svc, b := testService(t, adapter, now)

	out, err := svc.Purchase(context.Background(), PurchaseRequest{
		BindingID: b.ID, AgentID: "agent-a", Items: []purchasing.PurchaseItem{{ProductID: "p-2"}},
		Currency: "TWD", Amount: 799, IdempotencyKey: "idem-7",
	})
	if err != nil || out.Status != purchasing.PurchaseRejected || !strings.HasPrefix(out.Error, "CATEGORY_NOT_ALLOWED") {
		t.Fatalf("expected category rejection, got err=%v out=%+v", err, out)
	}
	if out.Status != purchasing.PurchaseRejected {
		t.Fatalf("status = %s, want REJECTED", out.Status)
	}
	if len(adapter.calls) != 1 {
		t.Fatalf("order placed despite rejection: %v", adapter.calls)
	}
}