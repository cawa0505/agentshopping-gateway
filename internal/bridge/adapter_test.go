package bridge

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/purchasing"
)

// fakeBridge serves the three order endpoints the adapter consumes.
func fakeBridge(t *testing.T, total float64, orderID int, status string, calls *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders/quote", func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, "quote")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quote_id":"qt_1","items":[{"product_id":10,"qty":2}],"total":700,"currency":"TWD","categories":["15"],"expires_at":"2099-01-01T00:00:00Z"}`))
	})
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, "create")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"order_id":42,"status":"processing","total":700,"currency":"TWD"}`))
	})
	mux.HandleFunc("GET /orders/status", func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, "status")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"` + status + `"}`))
	})
	mux.HandleFunc("POST /orders/cancel", func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, "cancel")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"cancelled"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestAdapter(t *testing.T, srv *httptest.Server) *WooCommerceAdapter {
	t.Helper()
	return NewWooCommerceAdapter(New(Config{BaseURL: srv.URL, APIKey: "k", StoreID: "shop1"}))
}

func TestAdapterQuoteMapsCanonicalFields(t *testing.T) {
	var calls []string
	adapter := newTestAdapter(t, fakeBridge(t, 700, 42, "processing", &calls))

	q, err := adapter.Quote([]purchasing.PurchaseItem{{ProductID: "10", Quantity: 2}})
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if q.QuoteID != "qt_1" || q.Total != 700 || q.Currency != "TWD" || q.MerchantID != "shop1" {
		t.Fatalf("quote = %+v", q)
	}
	if len(q.Categories) != 1 || q.Categories[0] != "15" {
		t.Fatalf("categories = %v", q.Categories)
	}
	if q.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expiry not parsed: %v", q.ExpiresAt)
	}
}

func TestAdapterCreateOrderVerifiesTotal(t *testing.T) {
	var calls []string
	adapter := newTestAdapter(t, fakeBridge(t, 700, 42, "processing", &calls))

	res, err := adapter.CreateOrder(&purchasing.PurchaseQuote{
		QuoteID: "qt_1", MerchantID: "shop1", Total: 700, Currency: "TWD",
		Items: []purchasing.PurchaseItem{{ProductID: "10", Quantity: 2}},
	}, "pm_ref", "agent-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !res.Success || res.MerchantOrderRef != "42" {
		t.Fatalf("result = %+v", res)
	}
}

func TestAdapterCreateOrderRejectsDivergentTotal(t *testing.T) {
	var calls []string
	srv := fakeBridge(t, 700, 42, "processing", &calls)
	// Patch create to return a divergent total.
	adapter := newTestAdapter(t, srv)

	res, err := adapter.CreateOrder(&purchasing.PurchaseQuote{
		QuoteID: "qt_1", MerchantID: "shop1", Total: 500, Currency: "TWD",
		Items: []purchasing.PurchaseItem{{ProductID: "10", Quantity: 2}},
	}, "pm_ref", "agent-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Success {
		t.Fatalf("divergent total must not succeed: %+v", res)
	}
	if res.ErrorMessage == "" {
		t.Fatalf("expected divergence error, got %+v", res)
	}
}

func TestAdapterGetOrderStatus(t *testing.T) {
	var calls []string
	adapter := newTestAdapter(t, fakeBridge(t, 700, 42, "completed", &calls))

	st, err := adapter.GetOrderStatus("42")
	if err != nil || st != "completed" {
		t.Fatalf("status = %q err=%v", st, err)
	}
	if _, err := adapter.GetOrderStatus("not-a-number"); err == nil {
		t.Fatalf("expected error for non-numeric ref")
	}
}
