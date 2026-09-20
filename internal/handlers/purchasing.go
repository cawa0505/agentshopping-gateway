// Purchasing module: delegated purchasing authorization over MCP. Auth
// middleware has already verified identity + the purchasing:pay ability.
// Domain rejections return ok:false plus an agent-actionable error code;
// responses never contain payment credentials (opaque refs only, spec 6.3).
package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cawa0505/agentshopping-gateway/internal/auth"
	"github.com/cawa0505/agentshopping-gateway/internal/purchasing"
	"github.com/cawa0505/agentshopping-gateway/internal/store"
)

// PurchasingModule dispatches the purchasing module actions:
// purchase / get_allowance / get_purchase.
func (d Deps) PurchasingModule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action         string                    `json:"action"`
		BindingID      string                    `json:"binding_id"`
		PurchaseID     string                    `json:"purchase_id"`
		MerchantID     string                    `json:"merchant_id"`
		Items          []purchasing.PurchaseItem `json:"items"`
		Currency       string                    `json:"currency"`
		Amount         int64                     `json:"amount"`
		IdempotencyKey string                    `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	claims, _ := auth.Claims(r.Context())
	agentID, _ := claims["sub"].(string)

	switch req.Action {
	case "purchase":
		d.purchasingPurchase(w, r, agentID, req)
	case "get_allowance":
		snap, err := d.Purchasing.GetAllowanceForAgent(r.Context(), req.BindingID, agentID)
		if err != nil {
			writePurchasingErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "allowance": snap})
	case "get_purchase":
		p, err := d.Purchasing.GetPurchaseForAgent(r.Context(), req.PurchaseID, agentID)
		if err != nil {
			writePurchasingErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "purchase": p})
	default:
		writeErr(w, http.StatusBadRequest, "unknown action")
	}
}

func (d Deps) purchasingPurchase(w http.ResponseWriter, r *http.Request, agentID string, req struct {
	Action         string                    `json:"action"`
	BindingID      string                    `json:"binding_id"`
	PurchaseID     string                    `json:"purchase_id"`
	MerchantID     string                    `json:"merchant_id"`
	Items          []purchasing.PurchaseItem `json:"items"`
	Currency       string                    `json:"currency"`
	Amount         int64                     `json:"amount"`
	IdempotencyKey string                    `json:"idempotency_key"`
}) {
	out, err := d.Purchasing.Purchase(r.Context(), store.PurchaseRequest{
		BindingID:      req.BindingID,
		AgentID:        agentID,
		MerchantID:     req.MerchantID,
		Items:          req.Items,
		Currency:       req.Currency,
		Amount:         req.Amount,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		writePurchasingErr(w, err)
		return
	}
	resp := map[string]any{
		"ok":               out.Error == "",
		"status":           string(out.Status),
		"idempotent_replay": out.IdempotentRe,
	}
	if out.Error != "" {
		resp["error"] = out.Error
	}
	if out.Purchase != nil {
		resp["purchase"] = out.Purchase
	}
	if out.Quote != nil {
		resp["quote"] = out.Quote
	}
	writeJSON(w, http.StatusOK, resp)
}

// writePurchasingErr maps domain errors to agent-actionable codes with
// matching HTTP semantics (spec 6.2).
func writePurchasingErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, purchasing.ErrBindingNotFound):
		code = http.StatusNotFound
	case errors.Is(err, purchasing.ErrBindingRevoked),
		errors.Is(err, purchasing.ErrBindingExpired),
		errors.Is(err, purchasing.ErrBindingSuspended),
		errors.Is(err, purchasing.ErrBindingAccessDenied):
		code = http.StatusForbidden
	case errors.Is(err, purchasing.ErrIdempotencyConflict):
		code = http.StatusConflict
	}
	writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
}
