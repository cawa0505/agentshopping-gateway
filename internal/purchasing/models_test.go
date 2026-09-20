package purchasing

import (
	"errors"
	"testing"
	"time"
)

func TestAuthBinding_Transition(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name       string
		binding    AuthBinding
		target     BindingStatus
		wantStatus BindingStatus
		wantErr    error
	}{
		{
			name:       "active -> revoked",
			binding:    AuthBinding{Status: BindingActive},
			target:     BindingRevoked,
			wantStatus: BindingRevoked,
		},
		{
			name:       "active -> suspended",
			binding:    AuthBinding{Status: BindingActive},
			target:     BindingSuspended,
			wantStatus: BindingSuspended,
		},
		{
			name:       "suspended -> active",
			binding:    AuthBinding{Status: BindingSuspended},
			target:     BindingActive,
			wantStatus: BindingActive,
		},
		{
			name:       "revoked cannot transition",
			binding:    AuthBinding{Status: BindingRevoked},
			target:     BindingActive,
			wantErr:    ErrInvalidStateTransition,
			wantStatus: BindingRevoked,
		},
		{
			name:       "expired cannot transition",
			binding:    AuthBinding{Status: BindingExpired},
			target:     BindingRevoked,
			wantErr:    ErrInvalidStateTransition,
			wantStatus: BindingExpired,
		},
		{
			name:       "active expires (legal terminal transition)",
			binding:    AuthBinding{Status: BindingActive},
			target:     BindingExpired,
			wantStatus: BindingExpired,
		},
		{
			name:       "revoked cannot suspend",
			binding:    AuthBinding{Status: BindingRevoked},
			target:     BindingSuspended,
			wantErr:    ErrInvalidStateTransition,
			wantStatus: BindingRevoked,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.binding.Transition(tc.target, now)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error %v, got %v", tc.wantErr, err)
				}
				if tc.binding.Status != tc.wantStatus {
					t.Fatalf("expected status %s, got %s", tc.wantStatus, tc.binding.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.binding.Status != tc.wantStatus {
				t.Fatalf("expected status %s, got %s", tc.wantStatus, tc.binding.Status)
			}
			if tc.target == BindingRevoked {
				if tc.binding.RevokedAt == nil {
					t.Fatal("revoked_at should be set")
				}
			}
		})
	}
}

func TestPurchase_Transition(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name       string
		purchase   Purchase
		target     PurchaseStatus
		wantStatus PurchaseStatus
		wantErr    error
	}{
		{"requested -> authorized", Purchase{Status: PurchaseRequested}, PurchaseAuthorized, PurchaseAuthorized, nil},
		{"requested -> rejected", Purchase{Status: PurchaseRequested}, PurchaseRejected, PurchaseRejected, nil},
		{"authorized -> reserved", Purchase{Status: PurchaseAuthorized}, PurchaseReserved, PurchaseReserved, nil},
		{"authorized -> rejected", Purchase{Status: PurchaseAuthorized}, PurchaseRejected, PurchaseRejected, nil},
		{"reserved -> executing", Purchase{Status: PurchaseReserved}, PurchaseExecuting, PurchaseExecuting, nil},
		{"reserved -> released", Purchase{Status: PurchaseReserved}, PurchaseReleased, PurchaseReleased, nil},
		{"executing -> captured", Purchase{Status: PurchaseExecuting}, PurchaseCaptured, PurchaseCaptured, nil},
		{"executing -> released", Purchase{Status: PurchaseExecuting}, PurchaseReleased, PurchaseReleased, nil},
		{"captured -> refunded", Purchase{Status: PurchaseCaptured}, PurchaseRefunded, PurchaseRefunded, nil},
		{"captured cannot go to released", Purchase{Status: PurchaseCaptured}, PurchaseReleased, PurchaseCaptured, ErrInvalidStateTransition},
		{"requested cannot go to captured", Purchase{Status: PurchaseRequested}, PurchaseCaptured, PurchaseRequested, ErrInvalidStateTransition},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.purchase.Transition(tc.target, now)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error %v, got %v", tc.wantErr, err)
				}
				if tc.purchase.Status != tc.wantStatus {
					t.Fatalf("expected status %s, got %s", tc.wantStatus, tc.purchase.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.purchase.Status != tc.wantStatus {
				t.Fatalf("expected status %s, got %s", tc.wantStatus, tc.purchase.Status)
			}
			if tc.purchase.UpdatedAt != now {
				t.Fatal("updated_at should be set")
			}
		})
	}
}