package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/purchasing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testBinding(t *testing.T, s *Store, opts func(*purchasing.AuthBinding, *purchasing.AuthorizationPolicy)) *purchasing.AuthBinding {
	t.Helper()
	now := time.Now().UTC()
	b := &purchasing.AuthBinding{
		ID:               NewID("ab_"),
		UserID:           "usr_1",
		PaymentMethodRef: "pm_store_abc",
		Currency:         "TWD",
		Status:           purchasing.BindingActive,
		MerchantID:       "wc-shop-1",
		CreatedAt:        now,
	}
	p := &purchasing.AuthorizationPolicy{
		ID:              NewID("pl_"),
		BindingID:       b.ID,
		AllowanceAmount: 5000,
		PerOrder:        1500,
		Daily:           3000,
		Monthly:         10000,
		ScopeCategories: []string{"books"},
		ApprovalMode:    "auto",
		CreatedAt:       now,
	}
	if opts != nil {
		opts(b, p)
	}
	if err := s.CreateBinding(context.Background(), b, p); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	return b
}

func reserveIn(t *testing.T, s *Store, b *purchasing.AuthBinding, amount int64, key, agent string, at time.Time) (*ReserveResult, error) {
	t.Helper()
	return s.Reserve(context.Background(), ReserveInput{
		BindingID:      b.ID,
		QuoteID:        "qt_1",
		IdempotencyKey: key,
		AgentID:        agent,
		Currency:       b.Currency,
		Amount:         amount,
		RequestedAt:    at,
		MerchantID:     b.MerchantID,
		Categories:     []string{"books"},
		ProductIDs:     []string{"p-1"},
	})
}

func TestCreateBindingWritesGrant(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, nil)

	snap, err := s.Allowance(context.Background(), b.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("allowance: %v", err)
	}
	if snap.Granted != 5000 || snap.Available != 5000 {
		t.Fatalf("expected granted/available 5000, got %+v", snap)
	}
	events, err := s.LedgerEvents(context.Background(), b.ID)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if len(events) != 1 || events[0].EventType != purchasing.LedgerEventGrant || events[0].Amount != 5000 {
		t.Fatalf("expected single grant event, got %+v", events)
	}
}

func TestReserveAndCaptureFlow(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, nil)
	now := time.Now().UTC()

	res, err := reserveIn(t, s, b, 799, "idem-1", "agent-a", now)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if res.IdempotentRe {
		t.Fatal("first reserve must not be replay")
	}
	if res.Purchase.Status != purchasing.PurchaseReserved || res.Reservation.Status != purchasing.ReservationActive {
		t.Fatalf("unexpected statuses: %+v / %+v", res.Purchase, res.Reservation)
	}

	snap, _ := s.Allowance(context.Background(), b.ID, now)
	if snap.Available != 5000-799 {
		t.Fatalf("available after reserve: got %d want %d", snap.Available, 5000-799)
	}

	if err := s.CaptureReservation(context.Background(), res.Purchase.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("capture: %v", err)
	}
	snap, _ = s.Allowance(context.Background(), b.ID, now.Add(time.Minute))
	if snap.Captured != 799 || snap.Available != 5000-799 {
		t.Fatalf("after capture: %+v", snap)
	}
}

func TestIdempotentReplayReturnsSamePurchase(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, nil)
	now := time.Now().UTC()

	first, err := reserveIn(t, s, b, 500, "idem-x", "agent-a", now)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	replay, err := reserveIn(t, s, b, 500, "idem-x", "agent-a", now.Add(time.Second))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.IdempotentRe || replay.Purchase.ID != first.Purchase.ID {
		t.Fatalf("replay mismatch: %+v vs %+v", replay.Purchase, first.Purchase)
	}
	// No duplicate side effects.
	snap, _ := s.Allowance(context.Background(), b.ID, now)
	if snap.ActiveReserves != 500 {
		t.Fatalf("duplicate reservation created: %+v", snap)
	}
}

func TestIdempotencyConflictOnDifferentParams(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, nil)
	now := time.Now().UTC()

	if _, err := reserveIn(t, s, b, 500, "idem-c", "agent-a", now); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	_, err := reserveIn(t, s, b, 900, "idem-c", "agent-a", now)
	if !errors.Is(err, purchasing.ErrIdempotencyConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestConcurrentReservationsNoOverspend(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, func(b *purchasing.AuthBinding, p *purchasing.AuthorizationPolicy) {
		p.AllowanceAmount = 1000
		p.PerOrder = 800
		p.Daily = 0
		p.Monthly = 0
	})
	now := time.Now().UTC()

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := reserveIn(t, s, b, 800, "conc-"+string(rune('a'+i)), "agent-a", now)
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if successes != 1 {
		t.Fatalf("expected exactly 1 success, got %d", successes)
	}
	snap, err := s.Allowance(context.Background(), b.ID, now)
	if err != nil {
		t.Fatalf("allowance: %v", err)
	}
	if snap.Available != 200 || snap.ActiveReserves != 800 {
		t.Fatalf("invariant violated: %+v", snap)
	}
}

func TestReleaseOnMerchantFailure(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, nil)
	now := time.Now().UTC()

	res, err := reserveIn(t, s, b, 1200, "idem-f", "agent-a", now)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := s.ReleaseReservation(context.Background(), res.Purchase.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("release: %v", err)
	}
	snap, _ := s.Allowance(context.Background(), b.ID, now.Add(time.Second))
	if snap.Available != 5000 || snap.ActiveReserves != 0 {
		t.Fatalf("allowance not restored: %+v", snap)
	}
	if res.Purchase.Status != purchasing.PurchaseReserved {
		// purchase row status checked below via fresh load
	}
	p, err := s.GetPurchase(context.Background(), res.Purchase.ID)
	if err != nil {
		t.Fatalf("get purchase: %v", err)
	}
	if p.Status != purchasing.PurchaseReleased {
		t.Fatalf("purchase status = %s, want RELEASED", p.Status)
	}
}

func TestRefundRestoresAllowance(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, nil)
	now := time.Now().UTC()

	res, _ := reserveIn(t, s, b, 700, "idem-r", "agent-a", now)
	if err := s.CaptureReservation(context.Background(), res.Purchase.ID, now); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := s.RefundPurchase(context.Background(), res.Purchase.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("refund: %v", err)
	}
	snap, _ := s.Allowance(context.Background(), b.ID, now.Add(time.Minute))
	if snap.Available != 5000 || snap.Refunded != 700 {
		t.Fatalf("refund not accounted: %+v", snap)
	}
}

func TestRevokeReleasesActiveReservations(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, nil)
	now := time.Now().UTC()

	res, _ := reserveIn(t, s, b, 1000, "idem-v", "agent-a", now)
	if err := s.RevokeBinding(context.Background(), b.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// New reservations rejected.
	_, err := reserveIn(t, s, b, 100, "idem-v2", "agent-a", now.Add(2*time.Second))
	if !errors.Is(err, purchasing.ErrBindingRevoked) {
		t.Fatalf("expected revoked, got %v", err)
	}
	snap, _ := s.Allowance(context.Background(), b.ID, now.Add(2*time.Second))
	if snap.ActiveReserves != 0 || snap.Available != 5000 {
		t.Fatalf("revocation did not release: %+v", snap)
	}
	_ = res
}

func TestExpiredReservationReclaimed(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, nil)
	now := time.Now().UTC()

	res, err := reserveIn(t, s, b, 900, "idem-e", "agent-a", now)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	later := now.Add(DefaultReservationTTL + time.Minute)
	released, err := s.ReclaimExpiredReservations(context.Background(), later)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if released != 1 {
		t.Fatalf("released %d, want 1", released)
	}
	snap, _ := s.Allowance(context.Background(), b.ID, later)
	if snap.Available != 5000 {
		t.Fatalf("allowance not restored after reclaim: %+v", snap)
	}
	// Purchase went to RELEASED.
	p, _ := s.GetPurchase(context.Background(), res.Purchase.ID)
	if p.Status != purchasing.PurchaseReleased {
		t.Fatalf("purchase status = %s, want RELEASED", p.Status)
	}
}

func TestPolicyLimitsReject(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, nil)
	now := time.Now().UTC()

	// Per-order cap 1500.
	if _, err := reserveIn(t, s, b, 1600, "idem-po", "agent-a", now); !errors.Is(err, purchasing.ErrPerOrderLimitExceeded) {
		t.Fatalf("per-order: got %v", err)
	}
	// Daily cap 3000: 1200 + 1200 + 700 should hit daily limit.
	if _, err := reserveIn(t, s, b, 1200, "idem-d1", "agent-a", now); err != nil {
		t.Fatalf("d1: %v", err)
	}
	if _, err := reserveIn(t, s, b, 1200, "idem-d2", "agent-a", now); err != nil {
		t.Fatalf("d2: %v", err)
	}
	if _, err := reserveIn(t, s, b, 700, "idem-d3", "agent-a", now); !errors.Is(err, purchasing.ErrDailyLimitExceeded) {
		t.Fatalf("daily: got %v", err)
	}
	// Category allow-list denies electronics.
	_, err := s.Reserve(context.Background(), ReserveInput{
		BindingID: b.ID, QuoteID: "qt", IdempotencyKey: "idem-cat", AgentID: "agent-a",
		Currency: "TWD", Amount: 100, RequestedAt: now,
		MerchantID: b.MerchantID, Categories: []string{"electronics"},
	})
	if !errors.Is(err, purchasing.ErrCategoryNotAllowed) {
		t.Fatalf("category: got %v", err)
	}
	// Rejected purchases leave no reservation side effects.
	snap, _ := s.Allowance(context.Background(), b.ID, now)
	if snap.ActiveReserves != 2400 {
		t.Fatalf("unexpected reservations: %+v", snap)
	}
}

func TestReserveExpiredBindingRejected(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()
	b := testBinding(t, s, func(b *purchasing.AuthBinding, _ *purchasing.AuthorizationPolicy) {
		exp := now.Add(-time.Hour)
		b.ExpiresAt = &exp
	})
	_, err := reserveIn(t, s, b, 100, "idem-exp", "agent-a", now)
	if !errors.Is(err, purchasing.ErrBindingExpired) {
		t.Fatalf("expected expired, got %v", err)
	}
}

func TestAllowanceRebuiltFromLedger(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, nil)
	now := time.Now().UTC()

	r1, _ := reserveIn(t, s, b, 300, "k1", "a", now)
	if err := s.CaptureReservation(context.Background(), r1.Purchase.ID, now); err != nil {
		t.Fatalf("capture: %v", err)
	}
	r2, _ := reserveIn(t, s, b, 400, "k2", "a", now)
	if err := s.CaptureReservation(context.Background(), r2.Purchase.ID, now); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := s.RefundPurchase(context.Background(), r2.Purchase.ID, now); err != nil {
		t.Fatalf("refund: %v", err)
	}
	r3, _ := reserveIn(t, s, b, 250, "k3", "a", now)

	snap, _ := s.Allowance(context.Background(), b.ID, now)
	// 5000 granted - 300+400 captured + 400 refunded - 250 active = 4450
	if snap.Available != 4450 {
		t.Fatalf("derived available = %d, want 4450 (%+v)", snap.Available, snap)
	}
	_ = r3
}
