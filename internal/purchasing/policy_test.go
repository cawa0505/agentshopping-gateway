package purchasing

import (
	"errors"
	"testing"
	"time"
)

func TestPolicyEvaluator_Evaluate(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour)

	tests := []struct {
		name    string
		input   EvaluationInput
		wantErr error
	}{
		{
			name: "valid purchase",
			input: EvaluationInput{
				Binding:      &AuthBinding{Status: BindingActive, Currency: "TWD", MerchantID: "wc-shop-1"},
				Policy:       &AuthorizationPolicy{AllowanceAmount: 5000, PerOrder: 1500, Daily: 3000, Monthly: 10000},
				Amount: 799, Currency: "TWD", MerchantID: "wc-shop-1",
				DailySpent: 1000, MonthlySpent: 2000, AvailableBal: 4000, CurrentTime: now,
			},
		},
		{
			name: "binding revoked",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingRevoked, Currency: "TWD"},
				Policy: &AuthorizationPolicy{AllowanceAmount: 5000},
				Amount: 100, Currency: "TWD", AvailableBal: 5000, CurrentTime: now,
			},
			wantErr: ErrBindingRevoked,
		},
		{
			name: "binding expired by status",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingExpired, Currency: "TWD"},
				Policy: &AuthorizationPolicy{AllowanceAmount: 5000},
				Amount: 100, Currency: "TWD", AvailableBal: 5000, CurrentTime: now,
			},
			wantErr: ErrBindingExpired,
		},
		{
			name: "binding expired by expires_at",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingActive, Currency: "TWD", ExpiresAt: &exp},
				Policy: &AuthorizationPolicy{AllowanceAmount: 5000},
				Amount: 100, Currency: "TWD", AvailableBal: 5000, CurrentTime: exp,
			},
			wantErr: ErrBindingExpired,
		},
		{
			name: "currency mismatch",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingActive, Currency: "TWD"},
				Policy: &AuthorizationPolicy{AllowanceAmount: 5000},
				Amount: 100, Currency: "USD", AvailableBal: 5000, CurrentTime: now,
			},
			wantErr: ErrCurrencyNotSupported,
		},
		{
			name: "per order limit exceeded",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingActive, Currency: "TWD"},
				Policy: &AuthorizationPolicy{PerOrder: 1500, Daily: 3000, Monthly: 10000, AllowanceAmount: 5000},
				Amount: 1600, Currency: "TWD", AvailableBal: 5000, CurrentTime: now,
			},
			wantErr: ErrPerOrderLimitExceeded,
		},
		{
			name: "daily limit exceeded",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingActive, Currency: "TWD"},
				Policy: &AuthorizationPolicy{PerOrder: 1500, Daily: 3000, Monthly: 10000, AllowanceAmount: 5000},
				Amount: 1000, Currency: "TWD", DailySpent: 2500, MonthlySpent: 0, AvailableBal: 5000, CurrentTime: now,
			},
			wantErr: ErrDailyLimitExceeded,
		},
		{
			name: "monthly limit exceeded",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingActive, Currency: "TWD"},
				Policy: &AuthorizationPolicy{PerOrder: 1500, Daily: 3000, Monthly: 10000, AllowanceAmount: 5000},
				Amount: 1000, Currency: "TWD", DailySpent: 0, MonthlySpent: 9500, AvailableBal: 5000, CurrentTime: now,
			},
			wantErr: ErrMonthlyLimitExceeded,
		},
		{
			name: "allowance exceeded",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingActive, Currency: "TWD"},
				Policy: &AuthorizationPolicy{AllowanceAmount: 5000},
				Amount: 1000, Currency: "TWD", AvailableBal: 999, CurrentTime: now,
			},
			wantErr: ErrAllowanceExceeded,
		},
		{
			name: "merchant not allowed",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingActive, Currency: "TWD", MerchantID: "wc-shop-1"},
				Policy: &AuthorizationPolicy{AllowanceAmount: 5000, ScopeMerchants: []string{"wc-shop-2"}},
				Amount: 100, Currency: "TWD", MerchantID: "wc-shop-1", AvailableBal: 5000, CurrentTime: now,
			},
			wantErr: ErrMerchantNotAllowed,
		},
		{
			name: "bound merchant mismatch",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingActive, Currency: "TWD", MerchantID: "wc-shop-1"},
				Policy: &AuthorizationPolicy{AllowanceAmount: 5000},
				Amount: 100, Currency: "TWD", MerchantID: "wc-shop-2", AvailableBal: 5000, CurrentTime: now,
			},
			wantErr: ErrMerchantNotAllowed,
		},
		{
			name: "category not allowed",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingActive, Currency: "TWD"},
				Policy: &AuthorizationPolicy{AllowanceAmount: 5000, ScopeCategories: []string{"books"}},
				Amount: 100, Currency: "TWD", Categories: []string{"electronics"}, AvailableBal: 5000, CurrentTime: now,
			},
			wantErr: ErrCategoryNotAllowed,
		},
		{
			name: "product not allowed",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingActive, Currency: "TWD"},
				Policy: &AuthorizationPolicy{AllowanceAmount: 5000, ScopeProducts: []string{"p-1"}},
				Amount: 100, Currency: "TWD", ProductIDs: []string{"p-2"}, AvailableBal: 5000, CurrentTime: now,
			},
			wantErr: ErrProductNotAllowed,
		},
		{
			name: "nil policy allowed",
			input: EvaluationInput{
				Binding: &AuthBinding{Status: BindingActive, Currency: "TWD"},
				Amount: 100, Currency: "TWD", AvailableBal: 5000, CurrentTime: now,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Evaluator{}).Evaluate(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}