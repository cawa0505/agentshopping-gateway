// Package pricing is the ledger-backed negotiation MVP: point redemption
// (atomic spend => discount) and transaction commission (earn on success).
// All balance mutations defer to NexusLedger's atomic endpoints; the gateway
// keeps no balance state of its own.
package pricing

import (
	"context"

	"github.com/cawa0505/agentshopping-gateway/internal/nexusledger"
)

// pointValue is the discount currency units granted per redeemed point.
//
// ponytail: fixed 1:1 redemption for the MVP. If tiered/eventful redemption
// rates are needed, make this a per-site rate looked up from NexusLedger.
const pointValue int64 = 1

// Service runs the pricing operations against a NexusLedger client.
type Service struct {
	nxl *nexusledger.Client
}

func New(nxl *nexusledger.Client) *Service { return &Service{nxl: nxl} }

// Balance reports the account's current point balance.
func (s *Service) Balance(ctx context.Context, account string) (int64, error) {
	return s.nxl.Balance(ctx, account)
}

// Redeem spends points and returns the discount applied plus the remaining
// balance. Insufficient balance surfaces nexusledger.ErrInsufficientBalance and
// applies no discount (the spend is atomic on the ledger side).
func (s *Service) Redeem(ctx context.Context, account string, points int64, orderRef string) (discount int64, remaining int64, err error) {
	if points <= 0 {
		return 0, 0, nil // nothing to redeem
	}
	remaining, err = s.nxl.Spend(ctx, account, points, orderRef)
	if err != nil {
		return 0, 0, err
	}
	return points * pointValue, remaining, nil
}

// Commission credits the referrer/platform account for a completed order. It is
// only called for successful transactions; a failed transaction never earns.
func (s *Service) Commission(ctx context.Context, account string, amount int64, orderRef string) (int64, error) {
	if amount <= 0 {
		return s.Balance(ctx, account)
	}
	return s.nxl.Earn(ctx, account, amount, orderRef)
}

// SettleOrder is the success-gated wrapper: commission is earned only when the
// order succeeded. A failed order is a no-op (returns current balance).
func (s *Service) SettleOrder(ctx context.Context, account string, amount int64, orderRef string, success bool) (int64, error) {
	if !success {
		return s.Balance(ctx, account)
	}
	return s.Commission(ctx, account, amount, orderRef)
}
