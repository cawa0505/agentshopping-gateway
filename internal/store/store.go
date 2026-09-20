// Package store provides SQLite persistence for the delegated purchasing
// authorization core. Closed-source boundary: this package backs
// internal/purchasing and must not be imported outside the gateway module.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/purchasing"

	// Pure-Go SQLite driver (same choice as NexusLedger; no cgo).
	_ "modernc.org/sqlite"
)

// schemaVersion of the embedded migration set.
const schemaVersion = 1

var migrations = []string{
	// v1: purchasing authorization tables.
	`CREATE TABLE IF NOT EXISTS auth_bindings (
		id                 TEXT PRIMARY KEY,
		user_id            TEXT NOT NULL,
		payment_method_ref TEXT NOT NULL,
		currency           TEXT NOT NULL,
		status             TEXT NOT NULL,
		merchant_id        TEXT NOT NULL DEFAULT '',
		created_at         INTEGER NOT NULL,
		expires_at         INTEGER,
		revoked_at         INTEGER
	)`,
	`CREATE TABLE IF NOT EXISTS auth_policies (
		id                  TEXT PRIMARY KEY,
		binding_id          TEXT NOT NULL REFERENCES auth_bindings(id),
		allowance_amount    INTEGER NOT NULL,
		per_order           INTEGER NOT NULL DEFAULT 0,
		daily               INTEGER NOT NULL DEFAULT 0,
		monthly             INTEGER NOT NULL DEFAULT 0,
		scope_merchants     TEXT,
		scope_categories    TEXT,
		scope_products      TEXT,
		approval_mode       TEXT NOT NULL DEFAULT 'auto',
		approval_max_amount INTEGER NOT NULL DEFAULT 0,
		created_at          INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS purchases (
		id                 TEXT PRIMARY KEY,
		binding_id         TEXT NOT NULL REFERENCES auth_bindings(id),
		quote_id           TEXT NOT NULL,
		idempotency_key    TEXT NOT NULL,
		agent_id           TEXT NOT NULL,
		currency           TEXT NOT NULL,
		amount             INTEGER NOT NULL,
		status             TEXT NOT NULL,
		merchant_order_ref TEXT,
		created_at         INTEGER NOT NULL,
		updated_at         INTEGER NOT NULL,
		UNIQUE(binding_id, idempotency_key)
	)`,
	`CREATE TABLE IF NOT EXISTS reservations (
		id         TEXT PRIMARY KEY,
		purchase_id TEXT NOT NULL REFERENCES purchases(id),
		binding_id  TEXT NOT NULL REFERENCES auth_bindings(id),
		amount      INTEGER NOT NULL,
		status      TEXT NOT NULL,
		expires_at  INTEGER NOT NULL,
		created_at  INTEGER NOT NULL
	)`,
	// Append-only ledger. Signed-convention deltas: grant/purchase_refunded/
	// purchase_voided store positive amounts; purchase_captured stores its
	// amount and is subtracted. Reservation events are audit-only (0 delta).
	`CREATE TABLE IF NOT EXISTS purchase_ledger (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		binding_id  TEXT NOT NULL REFERENCES auth_bindings(id),
		purchase_id TEXT,
		event_type  TEXT NOT NULL,
		amount      INTEGER NOT NULL,
		currency    TEXT NOT NULL,
		metadata    TEXT,
		created_at  INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ledger_binding ON purchase_ledger(binding_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_reservations_active ON reservations(binding_id, status, expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_purchases_updated ON purchases(status, updated_at)`,
}

// Store is the SQLite-backed persistence + authorization service for
// bindings, policies, purchases, reservations and the append-only ledger.
type Store struct {
	db *sql.DB
}

// New opens (creating if needed) the SQLite database at dsn and applies
// migrations.
func New(dsn string) (*Store, error) {
	// ponytail: MaxOpenConns(1) makes SQLite's single-writer model explicit
	// and serializes transactional reads+writes; swap to BEGIN IMMEDIATE +
	// row locks when this ever becomes a bottleneck.
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dsn, err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply %s: %w", pragma, err)
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	for i, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// NewID returns a random hex id Prefixed by caller (e.g. "ab_", "pu_").
func NewID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: crypto/rand unavailable: " + err.Error())
	}
	return prefix + hex.EncodeToString(b[:])
}

// AllowanceSnapshot is the derived allowance state of a binding.
type AllowanceSnapshot struct {
	Granted        int64 `json:"granted"`
	Captured       int64 `json:"captured"`
	Refunded       int64 `json:"refunded"`
	ActiveReserves int64 `json:"reserved"`
	Available      int64 `json:"available"`
}

// DefaultReservationTTL bounds how long a reservation holds allowance.
const DefaultReservationTTL = 15 * time.Minute

// CreateBinding persists an active AuthBinding with its AuthorizationPolicy
// and writes the GRANT ledger event, all atomically.
func (s *Store) CreateBinding(ctx context.Context, b *purchasing.AuthBinding, p *purchasing.AuthorizationPolicy) error {
	if b.Status != purchasing.BindingActive {
		return fmt.Errorf("create binding: initial status must be active, got %s", b.Status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO auth_bindings (id, user_id, payment_method_ref, currency, status, merchant_id, created_at, expires_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.UserID, b.PaymentMethodRef, b.Currency, string(b.Status), b.MerchantID,
		b.CreatedAt.Unix(), unixOpt(b.ExpiresAt), unixOpt(b.RevokedAt),
	); err != nil {
		return fmt.Errorf("insert binding: %w", err)
	}

	scopes, err := marshalScopes(p)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO auth_policies (id, binding_id, allowance_amount, per_order, daily, monthly,
		     scope_merchants, scope_categories, scope_products, approval_mode, approval_max_amount, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.BindingID, p.AllowanceAmount, p.PerOrder, p.Daily, p.Monthly,
		scopes.merchants, scopes.categories, scopes.products,
		nullIfEmpty(p.ApprovalMode), p.ApprovalMaxAmount, p.CreatedAt.Unix(),
	); err != nil {
		return fmt.Errorf("insert policy: %w", err)
	}

	if err := appendLedger(ctx, tx, b.ID, "", purchasing.LedgerEventGrant, p.AllowanceAmount, b.Currency, nowOr(b.CreatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// GetBinding loads a binding by id.
func (s *Store) GetBinding(ctx context.Context, id string) (*purchasing.AuthBinding, error) {
	return scanBinding(s.db.QueryRowContext(ctx,
		`SELECT id, user_id, payment_method_ref, currency, status, merchant_id, created_at, expires_at, revoked_at
		 FROM auth_bindings WHERE id = ?`, id))
}

// GetPolicy loads the policy attached to a binding.
func (s *Store) GetPolicy(ctx context.Context, bindingID string) (*purchasing.AuthorizationPolicy, error) {
	var (
		p                         purchasing.AuthorizationPolicy
		merch, cats, prods        sql.NullString
		approvalMode              sql.NullString
		created                   int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, binding_id, allowance_amount, per_order, daily, monthly,
		        scope_merchants, scope_categories, scope_products, approval_mode, approval_max_amount, created_at
		 FROM auth_policies WHERE binding_id = ?`, bindingID,
	).Scan(&p.ID, &p.BindingID, &p.AllowanceAmount, &p.PerOrder, &p.Daily, &p.Monthly,
		&merch, &cats, &prods, &approvalMode, &p.ApprovalMaxAmount, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // No policy attached; caller treats as evaluated-only checks.
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	p.ApprovalMode = approvalMode.String
	if p.ScopeMerchants, err = unmarshalScope(merch); err != nil {
		return nil, err
	}
	if p.ScopeCategories, err = unmarshalScope(cats); err != nil {
		return nil, err
	}
	if p.ScopeProducts, err = unmarshalScope(prods); err != nil {
		return nil, err
	}
	return &p, nil
}

// RevokeBinding moves a binding to revoked, releases its active reservations
// and writes the binding_revoked ledger event. Terminal; repeat calls error.
func (s *Store) RevokeBinding(ctx context.Context, id string, now time.Time) error {
	return s.terminalTransition(ctx, id, purchasing.BindingRevoked, purchasing.LedgerEventBindingRevoked, now)
}

// ExpireBindingDue lazily marks an active, overdue binding as expired.
// No-op (nil) when the binding is not due.
func (s *Store) ExpireBindingDue(ctx context.Context, id string, now time.Time) error {
	return s.terminalTransition(ctx, id, purchasing.BindingExpired, purchasing.LedgerEventBindingExpired, now)
}

func (s *Store) terminalTransition(ctx context.Context, id string, target purchasing.BindingStatus, ev purchasing.LedgerEventType, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	b, err := scanBinding(tx.QueryRowContext(ctx,
		`SELECT id, user_id, payment_method_ref, currency, status, merchant_id, created_at, expires_at, revoked_at
		 FROM auth_bindings WHERE id = ?`, id))
	if err != nil {
		return err
	}
	if err := b.Transition(target, now); err != nil {
		return err
	}
	revoked := int64(0)
	if b.RevokedAt != nil {
		revoked = b.RevokedAt.Unix()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE auth_bindings SET status = ?, revoked_at = ? WHERE id = ?`,
		string(b.Status), revoked, id); err != nil {
		return fmt.Errorf("update binding: %w", err)
	}

	if target == purchasing.BindingRevoked {
		// Release every still-active reservation; pending purchases follow.
		if err := releaseActiveReservations(ctx, tx, id, now); err != nil {
			return err
		}
	}
	if err := appendLedger(ctx, tx, id, "", ev, 0, b.Currency, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ReserveInput carries one authorization request.
type ReserveInput struct {
	BindingID       string
	QuoteID         string
	IdempotencyKey  string
	AgentID         string
	Currency        string
	Amount          int64 // minor units; must equal the verified quote total
	RequestedAt     time.Time
	ReservationTTL  time.Duration // defaults to DefaultReservationTTL
	// Evaluation inputs (recomputed server-side; never trusted from agent):
	MerchantID string
	Categories []string
	ProductIDs []string
}

// ReserveResult reports the outcome of a reservation attempt.
type ReserveResult struct {
	Purchase     *purchasing.Purchase
	Reservation  *purchasing.Reservation
	IdempotentRe bool // true when replayed from an existing idempotency key
}

// ValidateIdempotencyParams reports whether a replayed request matches the
// original request parameters for the same idempotency key.
func (r ReserveResult) ValidateIdempotencyParams(in ReserveInput) bool {
	p := r.Purchase
	return p != nil &&
		p.Amount == in.Amount &&
		p.Currency == in.Currency &&
		p.AgentID == in.AgentID &&
		p.QuoteID == in.QuoteID
}

// Reserve atomically evaluates policy and creates a purchase + reservation.
//
// Within a single transaction it: checks idempotency (replay returns the
// original result without new side effects), loads binding + policy, derives
// usage (daily/monthly/available), runs the domain Evaluator, persists the
// purchase (RESERVED or REJECTED) and, when authorized, an active reservation
// plus the reservation_created ledger event.
func (s *Store) Reserve(ctx context.Context, in ReserveInput) (*ReserveResult, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key required", purchasing.ErrPurchaseFailed)
	}
	ttl := in.ReservationTTL
	if ttl <= 0 {
		ttl = DefaultReservationTTL
	}
	now := in.RequestedAt

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Single-conn pool serializes writers; emulate immediate tx by touching
	// a write up front so row state below is stable until commit.
	if _, err := tx.ExecContext(ctx, `UPDATE purchase_ledger SET id = id WHERE 0`); err != nil {
		return nil, err
	}

	// 1. Idempotency check.
	existing, err := getPurchaseByKey(ctx, tx, in.BindingID, in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		res := &ReserveResult{Purchase: existing, IdempotentRe: true}
		if existing.Status == purchasing.PurchaseReserved || existing.Status == purchasing.PurchaseExecuting ||
			existing.Status == purchasing.PurchaseCaptured {
			res.Reservation, err = getActiveOrLatestReservation(ctx, tx, existing.ID)
			if err != nil {
				return nil, err
			}
		}
		if !res.ValidateIdempotencyParams(in) {
			return res, purchasing.ErrIdempotencyConflict
		}
		return res, nil
	}

	// 2. Load binding (+ lazy expiry) and policy.
	b, err := scanBinding(tx.QueryRowContext(ctx,
		`SELECT id, user_id, payment_method_ref, currency, status, merchant_id, created_at, expires_at, revoked_at
		 FROM auth_bindings WHERE id = ?`, in.BindingID))
	if err != nil {
		return nil, err
	}
	if b.Status == purchasing.BindingActive && b.ExpiresAt != nil && !now.Before(*b.ExpiresAt) {
		// Lazy expiry: reject; status flip happens via ExpireBindingDue sweep.
		return nil, purchasing.ErrBindingExpired
	}
	policy, err := getPolicyTx(ctx, tx, in.BindingID)
	if err != nil {
		return nil, err
	}

	// 3. Derive usage.
	snap, err := allowanceTx(ctx, tx, in.BindingID, now)
	if err != nil {
		return nil, err
	}
	daily, monthly, err := usageTx(ctx, tx, in.BindingID, now)
	if err != nil {
		return nil, err
	}

	// 4. Domain evaluation.
	purchase := &purchasing.Purchase{
		ID:             NewID("pu_"),
		BindingID:      in.BindingID,
		QuoteID:        in.QuoteID,
		IdempotencyKey: in.IdempotencyKey,
		AgentID:        in.AgentID,
		Currency:       in.Currency,
		Amount:         in.Amount,
		Status:         purchasing.PurchaseRequested,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	reject := func(err error) (*ReserveResult, error) {
		if errors.Is(err, purchasing.ErrBindingNotFound) || errors.Is(err, purchasing.ErrBindingRevoked) ||
			errors.Is(err, purchasing.ErrBindingExpired) || errors.Is(err, purchasing.ErrBindingSuspended) {
			return nil, err // No purchase record for missing/terminal bindings.
		}
		_ = purchase.Transition(purchasing.PurchaseRejected, now)
		if err := insertPurchase(ctx, tx, purchase); err != nil {
			return nil, err
		}
		return &ReserveResult{Purchase: purchase}, err
	}
	if b.Status != purchasing.BindingActive {
		return reject(terminalBindingErr(b.Status))
	}
	evaluator := &purchasing.Evaluator{}
	if err := evaluator.Evaluate(purchasing.EvaluationInput{
		Binding: b, Policy: policy,
		Amount: in.Amount, Currency: in.Currency,
		MerchantID: in.MerchantID, Categories: in.Categories, ProductIDs: in.ProductIDs,
		DailySpent: daily, MonthlySpent: monthly, AvailableBal: snap.Available,
		CurrentTime: now,
	}); err != nil {
		return reject(err)
	}

	// 5. Transition REQUESTED → AUTHORIZED → RESERVED and persist.
	if err := purchase.Transition(purchasing.PurchaseAuthorized, now); err != nil {
		return nil, err
	}
	if err := purchase.Transition(purchasing.PurchaseReserved, now); err != nil {
		return nil, err
	}
	if err := insertPurchase(ctx, tx, purchase); err != nil {
		if errors.Is(err, errUniqueConstraint) {
			// Raced on the same idempotency key: return the stored purchase.
			stored, gerr := getPurchaseByKey(ctx, tx, in.BindingID, in.IdempotencyKey)
			if gerr != nil {
				return nil, gerr
			}
			return &ReserveResult{Purchase: stored, IdempotentRe: true}, purchasing.ErrIdempotencyConflict
		}
		return nil, err
	}

	res := &purchasing.Reservation{
		ID:         NewID("rs_"),
		PurchaseID: purchase.ID,
		BindingID:  in.BindingID,
		Amount:     in.Amount,
		Status:     purchasing.ReservationActive,
		ExpiresAt:  now.Add(ttl),
		CreatedAt:  now,
	}
	if err := insertReservation(ctx, tx, res); err != nil {
		return nil, err
	}
	if err := appendLedger(ctx, tx, in.BindingID, purchase.ID,
		purchasing.LedgerEventReservationCreated, 0, in.Currency, now); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ReserveResult{Purchase: purchase, Reservation: res}, nil
}

// ReleaseReservation releases a purchase's active reservation (merchant
// failure path) and moves the purchase to RELEASED.
func (s *Store) ReleaseReservation(ctx context.Context, purchaseID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, purchase, err := reservationForPurchase(ctx, tx, purchaseID)
	if err != nil {
		return err
	}
	if res.Status != purchasing.ReservationActive {
		return fmt.Errorf("%w: reservation %s already %s", purchasing.ErrInvalidStateTransition, res.ID, res.Status)
	}
	if err := resTo(ctx, tx, res, purchasing.ReservationReleased); err != nil {
		return err
	}
	switch purchase.Status {
	case purchasing.PurchaseReserved:
		// fallthrough to transition below
	case purchasing.PurchaseExecuting:
		// merchant failure during execution
	default:
		return fmt.Errorf("%w: purchase %s in %s cannot release", purchasing.ErrInvalidStateTransition, purchaseID, purchase.Status)
	}
	if err := purchase.Transition(purchasing.PurchaseReleased, now); err != nil {
		return err
	}
	if err := persistPurchaseStatus(ctx, tx, purchase); err != nil {
		return err
	}
	if err := appendLedger(ctx, tx, purchase.BindingID, purchase.ID,
		purchasing.LedgerEventReservationReleased, 0, purchase.Currency, now); err != nil {
		return err
	}
	return tx.Commit()
}

// CaptureReservation confirms the merchant charge: reservation → captured,
// purchase → CAPTURED, ledger purchase_captured (-amount).
func (s *Store) CaptureReservation(ctx context.Context, purchaseID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, purchase, err := reservationForPurchase(ctx, tx, purchaseID)
	if err != nil {
		return err
	}
	if res.Status != purchasing.ReservationActive {
		return fmt.Errorf("%w: reservation %s not active (%s)", purchasing.ErrInvalidStateTransition, res.ID, res.Status)
	}
	switch purchase.Status {
	case purchasing.PurchaseReserved:
		if err := purchase.Transition(purchasing.PurchaseExecuting, now); err != nil {
			return err
		}
	case purchasing.PurchaseExecuting:
	default:
		return fmt.Errorf("%w: purchase %s in %s cannot capture", purchasing.ErrInvalidStateTransition, purchaseID, purchase.Status)
	}
	if err := purchase.Transition(purchasing.PurchaseCaptured, now); err != nil {
		return err
	}
	if err := persistPurchaseStatus(ctx, tx, purchase); err != nil {
		return err
	}
	if err := resTo(ctx, tx, res, purchasing.ReservationCaptured); err != nil {
		return err
	}
	if err := appendLedger(ctx, tx, purchase.BindingID, purchase.ID,
		purchasing.LedgerEventPurchaseCaptured, purchase.Amount, purchase.Currency, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RefundPurchase refunds a captured purchase (full amount in v1) and writes
// the purchase_refunded ledger event.
func (s *Store) RefundPurchase(ctx context.Context, purchaseID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	p, err := getPurchaseByID(ctx, tx, purchaseID)
	if err != nil {
		return err
	}
	if p.Status != purchasing.PurchaseCaptured {
		return fmt.Errorf("%w: purchase %s in %s cannot refund", purchasing.ErrInvalidStateTransition, purchaseID, p.Status)
	}
	if err := p.Transition(purchasing.PurchaseRefunded, now); err != nil {
		return err
	}
	if err := persistPurchaseStatus(ctx, tx, p); err != nil {
		return err
	}
	if err := appendLedger(ctx, tx, p.BindingID, p.ID,
		purchasing.LedgerEventPurchaseRefunded, p.Amount, p.Currency, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ReclaimExpiredReservations releases every active-but-overdue reservation.
// Returns the number of reservations released. Safe to run concurrently - it
// relies on the single-writer pool.
func (s *Store) ReclaimExpiredReservations(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, purchase_id FROM reservations
		 WHERE status = ? AND expires_at <= ?`, string(purchasing.ReservationActive), now.Unix())
	if err != nil {
		return 0, err
	}
	type due struct{ id, purchaseID string }
	var dueList []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.purchaseID); err != nil {
			rows.Close()
			return 0, err
		}
		dueList = append(dueList, d)
	}
	rows.Close()

	released := 0
	for _, d := range dueList {
		if err := s.ReleaseReservation(ctx, d.purchaseID, now); err != nil {
			return released, err
		}
		released++
	}
	return released, nil
}

// Allowance derives the binding's allowance snapshot:
// available = granted − captured + refunded − active_reservations  (≥ 0).
func (s *Store) Allowance(ctx context.Context, bindingID string, now time.Time) (*AllowanceSnapshot, error) {
	return allowanceTx(ctx, s.db, bindingID, now)
}

// GetPurchase loads a purchase by id.
func (s *Store) GetPurchase(ctx context.Context, id string) (*purchasing.Purchase, error) {
	p, err := getPurchaseByID(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, purchasing.ErrPurchaseFailed
	}
	return p, nil
}

// LedgerEvents loads the binding's ledger events, oldest first.
func (s *Store) LedgerEvents(ctx context.Context, bindingID string) ([]purchasing.LedgerEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, binding_id, COALESCE(purchase_id, ''), event_type, amount, currency, COALESCE(metadata, ''), created_at
		 FROM purchase_ledger WHERE binding_id = ? ORDER BY id ASC`, bindingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []purchasing.LedgerEntry
	for rows.Next() {
		var (
			e       purchasing.LedgerEntry
			created int64
		)
		if err := rows.Scan(&e.ID, &e.BindingID, &e.PurchaseID, &e.EventType, &e.Amount, &e.Currency, &e.Metadata, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- shared tx helpers -----------------------------------------------------

// allowanceTx computes the allowance snapshot inside the given querier's
// transaction/view so reads and writes stay consistent.
func allowanceTx(ctx context.Context, q queryer, bindingID string, now time.Time) (*AllowanceSnapshot, error) {
	snap := &AllowanceSnapshot{}
	if err := q.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN event_type = 'grant' THEN amount END), 0),
		   COALESCE(SUM(CASE WHEN event_type = 'purchase_captured' THEN amount END), 0),
		   COALESCE(SUM(CASE WHEN event_type IN ('purchase_refunded','purchase_voided') THEN amount END), 0)
		 FROM purchase_ledger WHERE binding_id = ?`, bindingID,
	).Scan(&snap.Granted, &snap.Captured, &snap.Refunded); err != nil {
		return nil, err
	}
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM reservations
		 WHERE binding_id = ? AND status = ? AND expires_at > ?`,
		bindingID, string(purchasing.ReservationActive), now.Unix(),
	).Scan(&snap.ActiveReserves); err != nil {
		return nil, err
	}
	snap.Available = snap.Granted - snap.Captured + snap.Refunded - snap.ActiveReserves
	if snap.Available < 0 {
		// Invariant I1: never expose negative availability.
		return nil, fmt.Errorf("%w: derived available %d below zero", purchasing.ErrReservationFailed, snap.Available)
	}
	return snap, nil
}

// usageTx returns spent-to-date (captured + active reservations) within the
// current UTC day and month windows for per-window limit checks.
func usageTx(ctx context.Context, q queryer, bindingID string, now time.Time) (daily int64, monthly int64, err error) {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// Captured amounts from ledger within windows.
	if err = q.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN created_at >= ? THEN amount END), 0),
		   COALESCE(SUM(CASE WHEN created_at >= ? THEN amount END), 0)
		 FROM purchase_ledger
		 WHERE binding_id = ? AND event_type = 'purchase_captured'`,
		dayStart.Unix(), monthStart.Unix(), bindingID,
	).Scan(&daily, &monthly); err != nil {
		return 0, 0, err
	}

	// Refunds within windows reduce usage.
	var refDaily, refMonthly int64
	if err = q.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN created_at >= ? THEN amount END), 0),
		   COALESCE(SUM(CASE WHEN created_at >= ? THEN amount END), 0)
		 FROM purchase_ledger
		 WHERE binding_id = ? AND event_type IN ('purchase_refunded','purchase_voided')`,
		dayStart.Unix(), monthStart.Unix(), bindingID,
	).Scan(&refDaily, &refMonthly); err != nil {
		return 0, 0, err
	}
	daily -= refDaily
	monthly -= refMonthly

	// Active (unexpired) reservations created within windows count toward usage.
	var resDaily, resMonthly int64
	if err = q.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN created_at >= ? THEN amount END), 0),
		   COALESCE(SUM(CASE WHEN created_at >= ? THEN amount END), 0)
		 FROM reservations
		 WHERE binding_id = ? AND status = ? AND expires_at > ?`,
		dayStart.Unix(), monthStart.Unix(), bindingID,
		string(purchasing.ReservationActive), now.Unix(),
	).Scan(&resDaily, &resMonthly); err != nil {
		return 0, 0, err
	}
	return daily + resDaily, monthly + resMonthly, nil
}

// releaseActiveReservations releases all active reservations of a binding
// inside the caller's transaction (used by RevokeBinding).
func releaseActiveReservations(ctx context.Context, tx *sql.Tx, bindingID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT r.id, r.purchase_id, p.status FROM reservations r
		 JOIN purchases p ON p.id = r.purchase_id
		 WHERE r.binding_id = ? AND r.status = ?`,
		bindingID, string(purchasing.ReservationActive))
	if err != nil {
		return err
	}
	type act struct {
		resID, purchaseID, purchaseStatus string
	}
	var acts []act
	for rows.Next() {
		var a act
		if err := rows.Scan(&a.resID, &a.purchaseID, &a.purchaseStatus); err != nil {
			rows.Close()
			return err
		}
		acts = append(acts, a)
	}
	rows.Close()

	for _, a := range acts {
		if _, err := tx.ExecContext(ctx,
			`UPDATE reservations SET status = ? WHERE id = ?`,
			string(purchasing.ReservationReleased), a.resID); err != nil {
			return err
		}
		// Only pending purchases flip to RELEASED; executing/captured stay.
		if a.purchaseStatus == string(purchasing.PurchaseReserved) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE purchases SET status = ?, updated_at = ? WHERE id = ?`,
				string(purchasing.PurchaseReleased), now.Unix(), a.purchaseID); err != nil {
				return err
			}
		}
		if err := appendLedger(ctx, tx, bindingID, a.purchaseID,
			purchasing.LedgerEventReservationReleased, 0, "", now); err != nil {
			return err
		}
	}
	return nil
}

func appendLedger(ctx context.Context, ex execer, bindingID, purchaseID string, ev purchasing.LedgerEventType, amount int64, currency string, at time.Time) error {
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO purchase_ledger (binding_id, purchase_id, event_type, amount, currency, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		bindingID, nullString(purchaseID), string(ev), amount, currency, at.Unix()); err != nil {
		return fmt.Errorf("append ledger %s: %w", ev, err)
	}
	return nil
}

func insertPurchase(ctx context.Context, ex execer, p *purchasing.Purchase) error {
	_, err := ex.ExecContext(ctx,
		`INSERT INTO purchases (id, binding_id, quote_id, idempotency_key, agent_id, currency, amount, status, merchant_order_ref, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
		p.ID, p.BindingID, p.QuoteID, p.IdempotencyKey, p.AgentID, p.Currency, p.Amount,
		string(p.Status), p.CreatedAt.Unix(), p.UpdatedAt.Unix())
	if err != nil && isUniqueViolation(err) {
		return errUniqueConstraint
	}
	return err
}

func insertReservation(ctx context.Context, ex execer, r *purchasing.Reservation) error {
	_, err := ex.ExecContext(ctx,
		`INSERT INTO reservations (id, purchase_id, binding_id, amount, status, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.PurchaseID, r.BindingID, r.Amount, string(r.Status), r.ExpiresAt.Unix(), r.CreatedAt.Unix())
	return err
}

func persistPurchaseStatus(ctx context.Context, ex execer, p *purchasing.Purchase) error {
	_, err := ex.ExecContext(ctx,
		`UPDATE purchases SET status = ?, updated_at = ?, merchant_order_ref = ? WHERE id = ?`,
		string(p.Status), p.UpdatedAt.Unix(), nullString(p.MerchantOrderRef), p.ID)
	return err
}

func getPurchaseByKey(ctx context.Context, q queryer, bindingID, key string) (*purchasing.Purchase, error) {
	return scanPurchase(q.QueryRowContext(ctx,
		`SELECT id, binding_id, quote_id, idempotency_key, agent_id, currency, amount, status, COALESCE(merchant_order_ref, ''), created_at, updated_at
		 FROM purchases WHERE binding_id = ? AND idempotency_key = ?`, bindingID, key))
}

func getPurchaseByID(ctx context.Context, q queryer, id string) (*purchasing.Purchase, error) {
	return scanPurchase(q.QueryRowContext(ctx,
		`SELECT id, binding_id, quote_id, idempotency_key, agent_id, currency, amount, status, COALESCE(merchant_order_ref, ''), created_at, updated_at
		 FROM purchases WHERE id = ?`, id))
}

func getActiveOrLatestReservation(ctx context.Context, q queryer, purchaseID string) (*purchasing.Reservation, error) {
	return scanReservation(q.QueryRowContext(ctx,
		`SELECT id, purchase_id, binding_id, amount, status, expires_at, created_at
		 FROM reservations WHERE purchase_id = ?
		 ORDER BY CASE status WHEN 'active' THEN 0 ELSE 1 END, created_at DESC LIMIT 1`, purchaseID))
}

func reservationForPurchase(ctx context.Context, q queryer, purchaseID string) (*purchasing.Reservation, *purchasing.Purchase, error) {
	res, err := getActiveOrLatestReservation(ctx, q, purchaseID)
	if err != nil {
		return nil, nil, err
	}
	p, err := getPurchaseByID(ctx, q, purchaseID)
	if err != nil {
		return nil, nil, err
	}
	return res, p, nil
}

func resTo(ctx context.Context, tx *sql.Tx, r *purchasing.Reservation, target purchasing.ReservationStatus) error {
	switch r.Status {
	case purchasing.ReservationActive:
	case purchasing.ReservationCaptured, purchasing.ReservationReleased:
		return fmt.Errorf("%w: reservation %s in %s", purchasing.ErrInvalidStateTransition, r.ID, r.Status)
	}
	_, err := tx.ExecContext(ctx, `UPDATE reservations SET status = ? WHERE id = ?`, string(target), r.ID)
	if err != nil {
		return err
	}
	r.Status = target
	return nil
}

func getPolicyTx(ctx context.Context, q queryer, bindingID string) (*purchasing.AuthorizationPolicy, error) {
	var (
		p                     purchasing.AuthorizationPolicy
		merch, cats, prods    sql.NullString
		approvalMode          sql.NullString
		created               int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, binding_id, allowance_amount, per_order, daily, monthly,
		        scope_merchants, scope_categories, scope_products, approval_mode, approval_max_amount, created_at
		 FROM auth_policies WHERE binding_id = ?`, bindingID,
	).Scan(&p.ID, &p.BindingID, &p.AllowanceAmount, &p.PerOrder, &p.Daily, &p.Monthly,
		&merch, &cats, &prods, &approvalMode, &p.ApprovalMaxAmount, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	p.ApprovalMode = approvalMode.String
	if p.ScopeMerchants, err = unmarshalScope(merch); err != nil {
		return nil, err
	}
	if p.ScopeCategories, err = unmarshalScope(cats); err != nil {
		return nil, err
	}
	if p.ScopeProducts, err = unmarshalScope(prods); err != nil {
		return nil, err
	}
	return &p, nil
}

// ---- scan helpers ----------------------------------------------------------

func scanBinding(row scanner) (*purchasing.AuthBinding, error) {
	var (
		b        purchasing.AuthBinding
		status   string
		created  int64
		exp      sql.NullInt64
		revoked  sql.NullInt64
	)
	if err := row.Scan(&b.ID, &b.UserID, &b.PaymentMethodRef, &b.Currency, &status, &b.MerchantID, &created, &exp, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, purchasing.ErrBindingNotFound
		}
		return nil, err
	}
	b.Status = purchasing.BindingStatus(status)
	b.CreatedAt = time.Unix(created, 0).UTC()
	if exp.Valid {
		t := time.Unix(exp.Int64, 0).UTC()
		b.ExpiresAt = &t
	}
	if revoked.Valid {
		t := time.Unix(revoked.Int64, 0).UTC()
		b.RevokedAt = &t
	}
	return &b, nil
}

func scanPurchase(row scanner) (*purchasing.Purchase, error) {
	var (
		p        purchasing.Purchase
		status   string
		created  int64
		updated  int64
	)
	if err := row.Scan(&p.ID, &p.BindingID, &p.QuoteID, &p.IdempotencyKey, &p.AgentID,
		&p.Currency, &p.Amount, &status, &p.MerchantOrderRef, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	p.Status = purchasing.PurchaseStatus(status)
	p.CreatedAt = time.Unix(created, 0).UTC()
	p.UpdatedAt = time.Unix(updated, 0).UTC()
	return &p, nil
}

func scanReservation(row scanner) (*purchasing.Reservation, error) {
	var (
		r       purchasing.Reservation
		status  string
		expires int64
		created int64
	)
	if err := row.Scan(&r.ID, &r.PurchaseID, &r.BindingID, &r.Amount, &status, &expires, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.Status = purchasing.ReservationStatus(status)
	r.ExpiresAt = time.Unix(expires, 0).UTC()
	r.CreatedAt = time.Unix(created, 0).UTC()
	return &r, nil
}

// ---- small helpers ---------------------------------------------------------

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type scanner interface {
	Scan(dest ...any) error
}

var errUniqueConstraint = errors.New("unique constraint violation")

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// modernc.org/sqlite reports constraint failures as "constraint failed:
	// UNIQUE constraint failed: purchases.binding_id, purchases.idempotency_key" (1555).
	var msg = err.Error()
	return containsUnique(msg)
}

func containsUnique(msg string) bool {
	for _, sub := range []string{"UNIQUE constraint failed", "unique constraint failed", "constraint failed"} {
		if len(msg) >= len(sub) && (msg == sub || searchStr(msg, sub)) {
			return true
		}
	}
	return false
}

func searchStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func terminalBindingErr(s purchasing.BindingStatus) error {
	switch s {
	case purchasing.BindingRevoked:
		return purchasing.ErrBindingRevoked
	case purchasing.BindingExpired:
		return purchasing.ErrBindingExpired
	case purchasing.BindingSuspended:
		return purchasing.ErrBindingSuspended
	default:
		return fmt.Errorf("%w: binding status %s", purchasing.ErrBindingNotFound, s)
	}
}

func unixOpt(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type scopes struct {
	merchants, categories, products driver.Value
}

func marshalScopes(p *purchasing.AuthorizationPolicy) (*scopes, error) {
	enc := func(list []string) (driver.Value, error) {
		if len(list) == 0 {
			return nil, nil
		}
		raw, err := json.Marshal(list)
		if err != nil {
			return nil, err
		}
		return string(raw), nil
	}
	out := &scopes{}
	var err error
	if out.merchants, err = enc(p.ScopeMerchants); err != nil {
		return nil, err
	}
	if out.categories, err = enc(p.ScopeCategories); err != nil {
		return nil, err
	}
	if out.products, err = enc(p.ScopeProducts); err != nil {
		return nil, err
	}
	return out, nil
}

func unmarshalScope(n sql.NullString) ([]string, error) {
	if !n.Valid || n.String == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(n.String), &out); err != nil {
		return nil, fmt.Errorf("unmarshal scope: %w", err)
	}
	return out, nil
}

func nowOr(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}
