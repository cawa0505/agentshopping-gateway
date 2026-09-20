// Package purchasing implements the closed-source core domain logic for
// AgentShop Delegated Purchasing Authorization / Auth Binding.
package purchasing

import (
	"errors"
	"fmt"
	"time"
)

// Standard Domain Errors
var (
	ErrBindingNotFound        = errors.New("BINDING_NOT_FOUND")
	ErrBindingRevoked         = errors.New("BINDING_REVOKED")
	ErrBindingExpired         = errors.New("BINDING_EXPIRED")
	ErrBindingSuspended       = errors.New("BINDING_SUSPENDED")
	ErrAllowanceExceeded      = errors.New("ALLOWANCE_EXCEEDED")
	ErrPerOrderLimitExceeded  = errors.New("PER_ORDER_LIMIT_EXCEEDED")
	ErrDailyLimitExceeded     = errors.New("DAILY_LIMIT_EXCEEDED")
	ErrMonthlyLimitExceeded   = errors.New("MONTHLY_LIMIT_EXCEEDED")
	ErrMerchantNotAllowed     = errors.New("MERCHANT_NOT_ALLOWED")
	ErrCategoryNotAllowed     = errors.New("CATEGORY_NOT_ALLOWED")
	ErrProductNotAllowed      = errors.New("PRODUCT_NOT_ALLOWED")
	ErrCurrencyNotSupported   = errors.New("CURRENCY_NOT_SUPPORTED")
	ErrReservationFailed      = errors.New("RESERVATION_FAILED")
	ErrPurchaseFailed         = errors.New("PURCHASE_FAILED")
	ErrQuoteExpired           = errors.New("QUOTE_EXPIRED")
	ErrIdempotencyConflict    = errors.New("IDEMPOTENCY_CONFLICT")
	ErrInvalidStateTransition = errors.New("INVALID_STATE_TRANSITION")
	ErrAmountMismatch         = errors.New("AMOUNT_MISMATCH")
	ErrBindingAccessDenied    = errors.New("BINDING_ACCESS_DENIED")
)

// BindingStatus represents the lifecycle state of an AuthBinding.
type BindingStatus string

const (
	BindingActive    BindingStatus = "active"
	BindingExpired   BindingStatus = "expired"
	BindingRevoked   BindingStatus = "revoked"
	BindingSuspended BindingStatus = "suspended"
)

// PurchaseStatus represents the lifecycle state of a Purchase.
type PurchaseStatus string

const (
	PurchaseRequested  PurchaseStatus = "REQUESTED"
	PurchaseAuthorized PurchaseStatus = "AUTHORIZED"
	PurchaseReserved   PurchaseStatus = "RESERVED"
	PurchaseExecuting  PurchaseStatus = "EXECUTING"
	PurchaseCaptured   PurchaseStatus = "CAPTURED"
	PurchaseReleased   PurchaseStatus = "RELEASED"
	PurchaseRejected   PurchaseStatus = "REJECTED"
	PurchaseRefunded   PurchaseStatus = "REFUNDED"
)

// ReservationStatus represents the lifecycle state of an Allowance Reservation.
type ReservationStatus string

const (
	ReservationActive   ReservationStatus = "active"
	ReservationReleased ReservationStatus = "released"
	ReservationCaptured ReservationStatus = "captured"
)

// LedgerEventType represents immutable event types in the purchasing ledger.
type LedgerEventType string

const (
	LedgerEventGrant               LedgerEventType = "grant"
	LedgerEventReservationCreated  LedgerEventType = "reservation_created"
	LedgerEventReservationReleased LedgerEventType = "reservation_released"
	LedgerEventPurchaseCaptured    LedgerEventType = "purchase_captured"
	LedgerEventPurchaseRefunded    LedgerEventType = "purchase_refunded"
	LedgerEventPurchaseVoided      LedgerEventType = "purchase_voided"
	LedgerEventBindingRevoked      LedgerEventType = "binding_revoked"
	LedgerEventBindingExpired      LedgerEventType = "binding_expired"
)

// AuthBinding represents a user's delegated purchasing authorization.
type AuthBinding struct {
	ID               string        `json:"id"`
	UserID           string        `json:"user_id"`
	AgentID          string        `json:"agent_id"` // Delegated agent's JWT sub; server-side enforced on every use
	PaymentMethodRef string        `json:"payment_method_ref"` // Opaque store/provider token
	Currency         string        `json:"currency"`           // e.g., "TWD", "USD"
	Status           BindingStatus `json:"status"`
	MerchantID       string        `json:"merchant_id"` // Bound merchant/store
	CreatedAt        time.Time     `json:"created_at"`
	ExpiresAt        *time.Time    `json:"expires_at,omitempty"`
	RevokedAt        *time.Time    `json:"revoked_at,omitempty"`
}

// Transition moves an AuthBinding to a new status according to state machine rules.
func (b *AuthBinding) Transition(target BindingStatus, now time.Time) error {
	if b.Status == BindingRevoked || b.Status == BindingExpired {
		return fmt.Errorf("%w: terminal state %s cannot transition to %s", ErrInvalidStateTransition, b.Status, target)
	}

	switch target {
	case BindingRevoked:
		b.Status = BindingRevoked
		b.RevokedAt = &now
		return nil
	case BindingExpired:
		b.Status = BindingExpired
		return nil
	case BindingSuspended:
		if b.Status != BindingActive {
			return fmt.Errorf("%w: cannot suspend binding from %s", ErrInvalidStateTransition, b.Status)
		}
		b.Status = BindingSuspended
		return nil
	case BindingActive:
		if b.Status != BindingSuspended {
			return fmt.Errorf("%w: cannot activate binding from %s", ErrInvalidStateTransition, b.Status)
		}
		b.Status = BindingActive
		return nil
	default:
		return fmt.Errorf("%w: unknown target state %s", ErrInvalidStateTransition, target)
	}
}

// AuthorizationPolicy defines spending limits and scope restrictions.
type AuthorizationPolicy struct {
	ID                string     `json:"id"`
	BindingID         string     `json:"binding_id"`
	AllowanceAmount   int64      `json:"allowance_amount"` // Total minor units
	PerOrder          int64      `json:"per_order"`        // Max per order, 0 = no limit
	Daily             int64      `json:"daily"`            // Max per day, 0 = no limit
	Monthly           int64      `json:"monthly"`          // Max per month, 0 = no limit
	ScopeMerchants    []string   `json:"scope_merchants"`  // Empty = unrestricted or fallback to binding
	ScopeCategories   []string   `json:"scope_categories"` // Category slugs
	ScopeProducts     []string   `json:"scope_products"`   // Specific product IDs
	ApprovalMode      string     `json:"approval_mode"`    // e.g., "auto"
	ApprovalMaxAmount int64      `json:"approval_max_amount"`
	CreatedAt         time.Time  `json:"created_at"`
}

// Purchase represents a single agent purchasing attempt.
type Purchase struct {
	ID               string         `json:"id"`
	BindingID        string         `json:"binding_id"`
	QuoteID          string         `json:"quote_id"`
	IdempotencyKey   string         `json:"idempotency_key"`
	AgentID          string         `json:"agent_id"` // JWT Subject
	Currency         string         `json:"currency"`
	Amount           int64          `json:"amount"` // Minor units
	Status           PurchaseStatus `json:"status"`
	MerchantOrderRef string         `json:"merchant_order_ref,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

// Transition moves a Purchase to a target state, ensuring correct lifecycle flow.
func (p *Purchase) Transition(target PurchaseStatus, now time.Time) error {
	valid := false
	switch p.Status {
	case PurchaseRequested:
		valid = (target == PurchaseAuthorized || target == PurchaseRejected)
	case PurchaseAuthorized:
		valid = (target == PurchaseReserved || target == PurchaseRejected)
	case PurchaseReserved:
		valid = (target == PurchaseExecuting || target == PurchaseReleased)
	case PurchaseExecuting:
		valid = (target == PurchaseCaptured || target == PurchaseReleased)
	case PurchaseCaptured:
		valid = (target == PurchaseRefunded)
	default:
		valid = false
	}

	if !valid {
		return fmt.Errorf("%w: invalid purchase transition from %s to %s", ErrInvalidStateTransition, p.Status, target)
	}

	p.Status = target
	p.UpdatedAt = now
	return nil
}

// Reservation records an active hold on allowance.
type Reservation struct {
	ID        string            `json:"id"`
	PurchaseID string           `json:"purchase_id"`
	BindingID  string           `json:"binding_id"`
	Amount     int64            `json:"amount"`
	Status     ReservationStatus `json:"status"`
	ExpiresAt  time.Time        `json:"expires_at"`
	CreatedAt  time.Time        `json:"created_at"`
}

// LedgerEntry records an immutable financial event for auditing and balance derivation.
type LedgerEntry struct {
	ID         int64           `json:"id"`
	BindingID  string          `json:"binding_id"`
	PurchaseID string          `json:"purchase_id,omitempty"`
	EventType  LedgerEventType `json:"event_type"`
	Amount     int64           `json:"amount"`
	Currency   string          `json:"currency"`
	Metadata   string          `json:"metadata,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// PurchaseItem represents an item requested by an agent.
type PurchaseItem struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

// PurchaseQuote represents a verified quote from a merchant adapter.
type PurchaseQuote struct {
	QuoteID    string            `json:"quote_id"`
	MerchantID string            `json:"merchant_id"`
	Items      []PurchaseItem   `json:"items"`
	Total      int64             `json:"total"` // Verified total amount
	Currency   string            `json:"currency"`
	Categories []string          `json:"categories,omitempty"` // For scope evaluation
	ExpiresAt  time.Time         `json:"expires_at"`
}

// PurchaseResult captures the outcome of merchant order placement.
type PurchaseResult struct {
	Success          bool   `json:"success"`
	MerchantOrderRef string `json:"merchant_order_ref,omitempty"`
	ErrorMessage     string `json:"error_message,omitempty"`
}

// MerchantAdapter defines the contract for merchant platform execution.
type MerchantAdapter interface {
	Quote(items []PurchaseItem) (*PurchaseQuote, error)
	CreateOrder(quote *PurchaseQuote, paymentMethodRef string, agentID string) (*PurchaseResult, error)
	GetOrderStatus(orderRef string) (string, error) // Returns standard order status: "pending", "processing", "completed", "failed"
}
