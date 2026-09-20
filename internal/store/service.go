// Package store provides the persistence-backed Purchasing Service
// that orchestrates the quote-first purchase lifecycle described in
// spec woocommerce-purchase-execution.
//
// The service does not trust agent-supplied monetary amounts: it
// re-queries the MerchantAdapter, verifies the total against the
// agent's declared amount, and only then reserves allowance and
// places the merchant order.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/purchasing"
)

// PurchaseRequest is the agent-facing purchase intent. The Amount
// field is declared by the agent and is verified against the quote.
type PurchaseRequest struct {
	BindingID      string
	AgentID        string
	MerchantID     string
	Items          []purchasing.PurchaseItem
	Currency       string
	Amount         int64 // agent-declared, must equal quote total
	IdempotencyKey string
}

// PurchaseOutcome is the result of a purchase attempt, including
// the derived error code when authorization failed.
type PurchaseOutcome struct {
	Purchase     *purchasing.Purchase
	Quote        *purchasing.PurchaseQuote
	Status       purchasing.PurchaseStatus
	Error        string // domain error code when rejected (empty on success)
	IdempotentRe bool
}

// Service orchestrates the lifecycle: quote → amount verification →
// atomic reservation → merchant order → capture/release.
type Service struct {
	Store   *Store
	Adapter purchasing.MerchantAdapter
	Now     func() time.Time
}

// NewService constructs a purchasing Service. Now defaults to time.Now
// when nil.
func NewService(store *Store, adapter purchasing.MerchantAdapter, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{Store: store, Adapter: adapter, Now: now}
}

// Purchase executes the full authorization flow and returns an
// outcome. On idempotent replay it returns the existing outcome
// without creating a new merchant order.
func (s *Service) Purchase(ctx context.Context, req PurchaseRequest) (*PurchaseOutcome, error) {
	now := s.Now()

	// 1. Quote-first: re-verify price from the merchant.
	quote, err := s.Adapter.Quote(req.Items)
	if err != nil {
		return &PurchaseOutcome{Status: purchasing.PurchaseRejected, Error: "PURCHASE_FAILED"}, err
	}
	if quote == nil {
		return &PurchaseOutcome{Status: purchasing.PurchaseRejected, Error: "PURCHASE_FAILED"}, fmt.Errorf("quote: nil")
	}
	if !now.Before(quote.ExpiresAt) {
		return &PurchaseOutcome{Status: purchasing.PurchaseRejected, Error: "QUOTE_EXPIRED"}, nil
	}
	if quote.Currency != req.Currency {
		return &PurchaseOutcome{Status: purchasing.PurchaseRejected, Error: "CURRENCY_NOT_SUPPORTED"}, nil
	}
	// Invariant 5: amount must come from the trusted quote, not the agent.
	if req.Amount != quote.Total {
		return &PurchaseOutcome{Status: purchasing.PurchaseRejected, Error: "AMOUNT_MISMATCH"}, nil
	}

	// 2. Load binding for payment_method_ref and policy checks.
	binding, err := s.Store.GetBinding(ctx, req.BindingID)
	if err != nil {
		return nil, err
	}
	if binding == nil {
		return &PurchaseOutcome{Status: purchasing.PurchaseRejected, Error: "BINDING_NOT_FOUND"}, nil
	}

	// 3. Atomic reservation (evaluates policy + allowance).
	res, err := s.Store.Reserve(ctx, ReserveInput{
		BindingID:      req.BindingID,
		QuoteID:        quote.QuoteID,
		IdempotencyKey: req.IdempotencyKey,
		AgentID:        req.AgentID,
		Currency:       req.Currency,
		Amount:         quote.Total,
		RequestedAt:    now,
		MerchantID:     quote.MerchantID,
		Categories:     quote.Categories,
		ProductIDs:     productIDs(req.Items),
	})
	if err != nil {
		if res != nil {
			return &PurchaseOutcome{Purchase: res.Purchase, Status: res.Purchase.Status, Error: err.Error()}, nil
		}
		return &PurchaseOutcome{Status: purchasing.PurchaseRejected, Error: err.Error()}, err
	}
	if res.IdempotentRe {
		// Replayed idempotency key: return the original outcome without
		// creating a new merchant order or side effects.
		return &PurchaseOutcome{Purchase: res.Purchase, IdempotentRe: res.IdempotentRe, Status: res.Purchase.Status, Error: outcomeError(res.Purchase.Status)}, nil
	}

	// 4. Place merchant order with the bound payment method token.
	order, err := s.Adapter.CreateOrder(quote, binding.PaymentMethodRef, req.AgentID)
	if err != nil {
		_ = s.Store.ReleaseReservation(ctx, res.Purchase.ID, now)
		return &PurchaseOutcome{Purchase: res.Purchase, Status: purchasing.PurchaseReleased, Error: "PURCHASE_FAILED"}, err
	}
	if !order.Success || order.MerchantOrderRef == "" {
		_ = s.Store.ReleaseReservation(ctx, res.Purchase.ID, now)
		return &PurchaseOutcome{Purchase: res.Purchase, Status: purchasing.PurchaseReleased, Error: order.ErrorMessage}, nil
	}

	// 5. Record the merchant order reference and enter EXECUTING.
	if err := s.Store.MarkExecuting(ctx, res.Purchase.ID, order.MerchantOrderRef, now); err != nil {
		return nil, err
	}
	res.Purchase.MerchantOrderRef = order.MerchantOrderRef
	res.Purchase.Status = purchasing.PurchaseExecuting

	// 6. Synchronous order-status check (v1 single poll; pending
	// orders leave reservation active for the reconcile sweep).
	status, err := s.Adapter.GetOrderStatus(order.MerchantOrderRef)
	if err != nil {
		return &PurchaseOutcome{Purchase: res.Purchase, Status: purchasing.PurchaseExecuting, Error: err.Error()}, nil
	}
	if status == "processing" || status == "completed" {
		if err := s.Store.CaptureReservation(ctx, res.Purchase.ID, now); err != nil {
			return nil, err
		}
		res.Purchase.Status = purchasing.PurchaseCaptured
		return &PurchaseOutcome{Purchase: res.Purchase, Status: purchasing.PurchaseCaptured}, nil
	}
	if status == "failed" || status == "cancelled" || status == "refunded" || status == "voided" {
		_ = s.Store.ReleaseReservation(ctx, res.Purchase.ID, now)
		return &PurchaseOutcome{Purchase: res.Purchase, Status: purchasing.PurchaseReleased, Error: "merchant " + status}, nil
	}
	// pending / on-hold: reservation stays active until reconcile.
	return &PurchaseOutcome{Purchase: res.Purchase, Status: purchasing.PurchaseExecuting}, nil
}

// ReconcileExecuting re-polls every EXECUTING purchase's merchant order
// status and settles it: paid/processing → CAPTURED, failed → RELEASED.
// Purchases still pending at the merchant are left in EXECUTING.
func (s *Service) ReconcileExecuting(ctx context.Context) ([]*purchasing.Purchase, error) {
	purchases, err := s.Store.ListExecuting(ctx)
	if err != nil {
		return nil, err
	}
	var settled []*purchasing.Purchase
	for _, p := range purchases {
		if p.MerchantOrderRef == "" {
			continue
		}
		status, err := s.Adapter.GetOrderStatus(p.MerchantOrderRef)
		if err != nil {
			continue // transient merchant error; retry next sweep
		}
		now := s.Now()
		switch status {
		case "processing", "completed", "paid":
			if err := s.Store.CaptureReservation(ctx, p.ID, now); err != nil {
				return settled, err
			}
			p.Status = purchasing.PurchaseCaptured
			settled = append(settled, p)
		case "failed", "cancelled", "trash":
			if err := s.Store.ReleaseReservation(ctx, p.ID, now); err != nil {
				return settled, err
			}
			p.Status = purchasing.PurchaseReleased
			settled = append(settled, p)
		}
	}
	return settled, nil
}

// GetAllowance returns the derived allowance snapshot for a binding.
func (s *Service) GetAllowance(ctx context.Context, bindingID string) (*AllowanceSnapshot, error) {
	return s.Store.Allowance(ctx, bindingID, s.Now())
}

// GetAllowanceForAgent returns the allowance snapshot only when the binding
// belongs to the requesting agent (spec 7.2: agents cannot probe bindings
// they are not delegated to).
func (s *Service) GetAllowanceForAgent(ctx context.Context, bindingID, agentID string) (*AllowanceSnapshot, error) {
	b, err := s.Store.GetBinding(ctx, bindingID)
	if err != nil {
		return nil, err
	}
	if b.AgentID != agentID {
		return nil, purchasing.ErrBindingAccessDenied
	}
	return s.Store.Allowance(ctx, bindingID, s.Now())
}

// GetPurchase returns a persisted purchase by id.
func (s *Service) GetPurchase(ctx context.Context, id string) (*purchasing.Purchase, error) {
	return s.Store.GetPurchase(ctx, id)
}

// GetPurchaseForAgent returns the purchase only when its binding belongs to
// the requesting agent (spec 7.2).
func (s *Service) GetPurchaseForAgent(ctx context.Context, id, agentID string) (*purchasing.Purchase, error) {
	p, err := s.Store.GetPurchase(ctx, id)
	if err != nil {
		return nil, err
	}
	b, err := s.Store.GetBinding(ctx, p.BindingID)
	if err != nil {
		return nil, err
	}
	if b.AgentID != agentID {
		return nil, purchasing.ErrBindingAccessDenied
	}
	return p, nil
}

func outcomeError(status purchasing.PurchaseStatus) string {
	switch status {
	case purchasing.PurchaseRejected:
		return "PURCHASE_REJECTED"
	case purchasing.PurchaseReleased:
		return "PURCHASE_RELEASED"
	default:
		return ""
	}
}

func productIDs(items []purchasing.PurchaseItem) []string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ProductID)
	}
	return ids
}