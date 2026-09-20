package purchasing

import (
	"fmt"
	"time"
)

// Evaluator checks whether a proposed purchase satisfies an AuthorizationPolicy.
type Evaluator struct{}

// EvaluationInput provides context required for policy validation.
type EvaluationInput struct {
	Binding       *AuthBinding
	Policy        *AuthorizationPolicy
	Amount        int64
	Currency      string
	MerchantID    string
	Categories    []string // Product categories belonging to the purchase items
	ProductIDs    []string
	DailySpent    int64     // Total spent/captured today in the given timezone/day
	MonthlySpent  int64     // Total spent/captured this month
	AvailableBal  int64     // Current derived available allowance: granted - captured - active_reservations
	CurrentTime   time.Time
}

// Evaluate performs all server-side policy and limit evaluations.
func (e *Evaluator) Evaluate(in EvaluationInput) error {
	if in.Binding == nil {
		return ErrBindingNotFound
	}

	// 1. Check Binding Status
	if in.Binding.Status == BindingRevoked {
		return ErrBindingRevoked
	}
	if in.Binding.Status == BindingExpired {
		return ErrBindingExpired
	}
	if in.Binding.Status == BindingSuspended {
		return ErrBindingSuspended
	}
	if in.Binding.Status != BindingActive {
		return fmt.Errorf("%w: binding status is %s", ErrBindingNotFound, in.Binding.Status)
	}

	// 2. Check Expiration
	if in.Binding.ExpiresAt != nil && !in.CurrentTime.Before(*in.Binding.ExpiresAt) {
		return ErrBindingExpired
	}

	// 3. Currency Check (v1 strictly rejects mismatches, no FX)
	if in.Currency != in.Binding.Currency {
		return fmt.Errorf("%w: requested %s does not match binding %s", ErrCurrencyNotSupported, in.Currency, in.Binding.Currency)
	}

	if in.Policy == nil {
		return nil
	}

	// 4. Per Order Limit
	if in.Policy.PerOrder > 0 && in.Amount > in.Policy.PerOrder {
		return fmt.Errorf("%w: amount %d exceeds per-order limit %d", ErrPerOrderLimitExceeded, in.Amount, in.Policy.PerOrder)
	}

	// 5. Daily Limit
	if in.Policy.Daily > 0 && (in.DailySpent+in.Amount) > in.Policy.Daily {
		return fmt.Errorf("%w: daily total %d exceeds daily limit %d", ErrDailyLimitExceeded, in.DailySpent+in.Amount, in.Policy.Daily)
	}

	// 6. Monthly Limit
	if in.Policy.Monthly > 0 && (in.MonthlySpent+in.Amount) > in.Policy.Monthly {
		return fmt.Errorf("%w: monthly total %d exceeds monthly limit %d", ErrMonthlyLimitExceeded, in.MonthlySpent+in.Amount, in.Policy.Monthly)
	}

	// 7. Total Available Allowance
	if in.Amount > in.AvailableBal {
		return fmt.Errorf("%w: amount %d exceeds available allowance %d", ErrAllowanceExceeded, in.Amount, in.AvailableBal)
	}

	// 8. Scope Checks (Deny-by-default if list is non-empty)
	// Merchant Scope
	if len(in.Policy.ScopeMerchants) > 0 {
		matched := false
		for _, m := range in.Policy.ScopeMerchants {
			if m == in.MerchantID {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%w: merchant %s not in scope", ErrMerchantNotAllowed, in.MerchantID)
		}
	} else if in.Binding.MerchantID != "" && in.Binding.MerchantID != in.MerchantID {
		// Fallback to binding's bound single merchant
		return fmt.Errorf("%w: merchant %s does not match bound merchant %s", ErrMerchantNotAllowed, in.MerchantID, in.Binding.MerchantID)
	}

	// Category Scope
	if len(in.Policy.ScopeCategories) > 0 {
		if len(in.Categories) == 0 {
			return fmt.Errorf("%w: purchase items have no category assigned", ErrCategoryNotAllowed)
		}
		allowedMap := make(map[string]bool)
		for _, c := range in.Policy.ScopeCategories {
			allowedMap[c] = true
		}
		for _, c := range in.Categories {
			if !allowedMap[c] {
				return fmt.Errorf("%w: category %s is not permitted", ErrCategoryNotAllowed, c)
			}
		}
	}

	// Product Scope
	if len(in.Policy.ScopeProducts) > 0 {
		if len(in.ProductIDs) == 0 {
			return fmt.Errorf("%w: no product IDs provided", ErrProductNotAllowed)
		}
		allowedMap := make(map[string]bool)
		for _, p := range in.Policy.ScopeProducts {
			allowedMap[p] = true
		}
		for _, p := range in.ProductIDs {
			if !allowedMap[p] {
				return fmt.Errorf("%w: product %s is not permitted", ErrProductNotAllowed, p)
			}
		}
	}

	return nil
}
