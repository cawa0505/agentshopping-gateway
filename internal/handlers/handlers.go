// Package handlers implements the standard protocol module endpoints by
// orchestrating the store bridge and the ledger pricing service. Auth has
// already run in middleware by the time these execute.
package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/cawa0505/agentshopping-gateway/internal/auth"
	"github.com/cawa0505/agentshopping-gateway/internal/bridge"
	"github.com/cawa0505/agentshopping-gateway/internal/pricing"
	"github.com/cawa0505/agentshopping-gateway/internal/store"
)

// Deps holds the collaborators the module handlers need.
type Deps struct {
	Bridge     *bridge.Client
	Pricing    *pricing.Service
	Purchasing *store.Service
}

// Catalog handles search + inventory reads (anonymous browsing allowed).
func (d Deps) Catalog(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keyword string `json:"keyword"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	products, err := d.Bridge.Search(r.Context(), req.Keyword)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": products})
}

// Cart handles add-to-cart mutations.
func (d Deps) Cart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cart      []bridge.CartLine `json:"cart"`
		ProductID int               `json:"product_id"`
		Qty       int               `json:"qty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	cart, subtotal, err := d.Bridge.AddToCart(r.Context(), req.Cart, req.ProductID, req.Qty)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"cart": cart, "subtotal": subtotal, "ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cart": cart, "subtotal": subtotal, "ok": true})
}

// Checkout optionally redeems points for a discount, then returns the store
// checkout URL. Point redemption is atomic on the ledger; a failed redemption
// aborts checkout so the shopper is never charged against phantom points.
func (d Deps) Checkout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cart         []bridge.CartLine `json:"cart"`
		Account      string            `json:"account"`
		RedeemPoints int64             `json:"redeem_points"`
		OrderRef     string            `json:"order_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}

	var discount int64
	if req.RedeemPoints > 0 {
		token, _ := auth.Token(r.Context())
		d2, _, err := d.Pricing.Redeem(r.Context(), token, req.Account, req.RedeemPoints, req.OrderRef)
		if err != nil {
			writeErr(w, http.StatusConflict, "redeem failed: "+err.Error())
			return
		}
		discount = d2
	}

	url, err := d.Bridge.Checkout(r.Context(), req.Cart)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": url, "discount": discount})
}

// PostOrder settles commission for a completed order. Failed orders earn nothing.
func (d Deps) PostOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account  string `json:"account"`
		Amount   int64  `json:"amount"`
		OrderRef string `json:"order_ref"`
		Success  bool   `json:"success"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if _, ok := auth.Claims(r.Context()); !ok {
		writeErr(w, http.StatusUnauthorized, "identity required")
		return
	}
	token, _ := auth.Token(r.Context())
	balance, err := d.Pricing.SettleOrder(r.Context(), token, req.Account, req.Amount, req.OrderRef, req.Success)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"balance": balance})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}
