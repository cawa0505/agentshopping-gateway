// Package bridge — WooCommerceAdapter implements purchasing.MerchantAdapter
// on top of a store's AgentShopping Bridge REST surface. The bridge is the
// trusted source for totals (invariant 5): the adapter verifies that the
// order the store actually created matches the quote the reservation was
// taken against, and cancels the order when they diverge.
package bridge

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/purchasing"
)

// WooCommerceAdapter adapts one store bridge to the purchasing contract.
type WooCommerceAdapter struct {
	client *Client
}

func NewWooCommerceAdapter(client *Client) *WooCommerceAdapter {
	return &WooCommerceAdapter{client: client}
}

// Quote fetches a canonical quote from the bridge (live store prices).
func (a *WooCommerceAdapter) Quote(items []purchasing.PurchaseItem) (*purchasing.PurchaseQuote, error) {
	bridgeItems := make([]OrderItem, 0, len(items))
	for _, it := range items {
		pid, err := strconv.Atoi(it.ProductID)
		if err != nil {
			return nil, fmt.Errorf("invalid product id %q: %w", it.ProductID, err)
		}
		bridgeItems = append(bridgeItems, OrderItem{ProductID: pid, Qty: it.Quantity})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	quoteID, total, currency, categories, expiresAt, err := a.client.QuoteOrder(ctx, bridgeItems)
	if err != nil {
		return nil, err
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("bridge quote expiry: %w", err)
	}
	return &purchasing.PurchaseQuote{
		QuoteID:    quoteID,
		MerchantID: a.client.cfg.StoreID,
		Items:      items,
		Total:      int64(math.Round(total)),
		Currency:   currency,
		Categories: categories,
		ExpiresAt:  exp,
	}, nil
}

// CreateOrder places the autonomous order and verifies the live total matches
// the quoted total before returning success. On divergence the order is
// cancelled and an error returned (the caller releases the reservation).
func (a *WooCommerceAdapter) CreateOrder(quote *purchasing.PurchaseQuote, paymentMethodRef, agentID string) (*purchasing.PurchaseResult, error) {
	bridgeItems := make([]OrderItem, 0, len(quote.Items))
	for _, it := range quote.Items {
		pid, err := strconv.Atoi(it.ProductID)
		if err != nil {
			return nil, fmt.Errorf("invalid product id %q: %w", it.ProductID, err)
		}
		bridgeItems = append(bridgeItems, OrderItem{ProductID: pid, Qty: it.Quantity})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	orderID, status, total, err := a.client.CreateOrder(ctx, bridgeItems, paymentMethodRef, agentID, quote.QuoteID)
	if err != nil {
		return &purchasing.PurchaseResult{Success: false, ErrorMessage: err.Error()}, nil
	}
	if int64(math.Round(total)) != quote.Total {
		// ponytail: best-effort cancel; a failed cancel leaves a store order to
		// expire via Woo's own cancellation — reconciliation is a Phase 3 concern.
		_ = a.client.CancelOrder(ctx, orderID)
		return &purchasing.PurchaseResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("order total %d diverges from quoted %d", int64(math.Round(total)), quote.Total),
		}, nil
	}
	_ = status // bridge status is advisory; capture is driven by GetOrderStatus polling
	return &purchasing.PurchaseResult{Success: true, MerchantOrderRef: strconv.Itoa(orderID)}, nil
}

// GetOrderStatus maps a store order to the canonical status set.
func (a *WooCommerceAdapter) GetOrderStatus(orderRef string) (string, error) {
	orderID, err := strconv.Atoi(orderRef)
	if err != nil {
		return "", fmt.Errorf("invalid order ref %q: %w", orderRef, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return a.client.OrderStatus(ctx, orderID)
}
