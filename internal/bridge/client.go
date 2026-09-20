// Package bridge is the gateway-side client for a store's AgentShopping Bridge
// plugin. The gateway calls the bridge's REST surface (search / inventory /
// cart / checkout) over HTTP, authenticating with a per-store shared secret.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Config points at one store's bridge endpoint.
type Config struct {
	BaseURL string // e.g. https://store.example/wp-json/agentshopping-bridge/v1
	APIKey  string // shared secret issued to the store
	StoreID string // merchant identifier used in purchasing policy scope
}

// Client calls a store bridge.
type Client struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Client { return &Client{cfg: cfg, http: http.DefaultClient} }

// Product mirrors the bridge product shape.
type Product struct {
	ID          int     `json:"id"`
	Name        string  `json:"name"`
	SKU         string  `json:"sku"`
	Price       float64 `json:"price"`
	Available   *int    `json:"available"`
	Purchasable bool    `json:"purchasable"`
}

// CartLine mirrors one bridge cart line.
type CartLine struct {
	ProductID int     `json:"product_id"`
	Name      string  `json:"name"`
	Qty       int     `json:"qty"`
	UnitPrice float64 `json:"unit_price"`
}

// Search returns products matching the keyword.
func (c *Client) Search(ctx context.Context, keyword string) ([]Product, error) {
	u := c.cfg.BaseURL + "/catalog/search?q=" + url.QueryEscape(keyword)
	var out struct {
		Products []Product `json:"products"`
	}
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	return out.Products, nil
}

// AddToCart posts a cart mutation and returns the resulting cart + subtotal.
func (c *Client) AddToCart(ctx context.Context, cart []CartLine, productID, qty int) ([]CartLine, float64, error) {
	body := map[string]any{"cart": cart, "product_id": productID, "qty": qty}
	var out struct {
		Cart     []CartLine `json:"cart"`
		Subtotal float64    `json:"subtotal"`
		OK       bool       `json:"ok"`
		Error    string     `json:"error"`
	}
	if err := c.postJSON(ctx, c.cfg.BaseURL+"/cart/add", body, &out); err != nil {
		return nil, 0, err
	}
	if !out.OK {
		return out.Cart, out.Subtotal, fmt.Errorf("bridge cart rejected: %s", out.Error)
	}
	return out.Cart, out.Subtotal, nil
}

// Checkout turns a cart into a store checkout URL.
func (c *Client) Checkout(ctx context.Context, cart []CartLine) (string, error) {
	body := map[string]any{"cart": cart}
	var out struct {
		URL   string `json:"url"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.postJSON(ctx, c.cfg.BaseURL+"/checkout", body, &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("bridge checkout rejected: %s", out.Error)
	}
	return out.URL, nil
}

// QuoteOrder fetches a canonical price quote from the bridge (live store
// prices; the trusted total source for the quote-first purchase flow).
func (c *Client) QuoteOrder(ctx context.Context, items []OrderItem) (quoteID string, total float64, currency string, categories []string, expiresAt string, err error) {
	body := map[string]any{"items": items}
	var out struct {
		QuoteID    string   `json:"quote_id"`
		Items      []OrderItem `json:"items"`
		Total      float64  `json:"total"`
		Currency   string   `json:"currency"`
		Categories []string `json:"categories"`
		ExpiresAt  string   `json:"expires_at"`
		Error      string   `json:"error"`
	}
	if err = c.postJSON(ctx, c.cfg.BaseURL+"/orders/quote", body, &out); err != nil {
		return "", 0, "", nil, "", err
	}
	if out.Error != "" || out.QuoteID == "" {
		return "", 0, "", nil, "", fmt.Errorf("bridge quote rejected: %s", out.Error)
	}
	return out.QuoteID, out.Total, out.Currency, out.Categories, out.ExpiresAt, nil
}

// OrderItem is one autonomous-order line as the bridge expects it.
type OrderItem struct {
	ProductID int `json:"product_id"`
	Qty       int `json:"qty"`
}

// NegotiateTier returns the bridge's tier (negotiated) quote verbatim —
// per-line unit_price/price_source detail included. The gateway passes the
// response through unchanged; the bridge is the pricing authority.
func (c *Client) NegotiateTier(ctx context.Context, items []OrderItem) (map[string]any, error) {
	body := map[string]any{"items": items}
	var out map[string]any
	if err := c.postJSON(ctx, c.cfg.BaseURL+"/catalog/negotiate", body, &out); err != nil {
		return nil, err
	}
	if msg, _ := out["error"].(string); msg != "" {
		return nil, fmt.Errorf("bridge negotiate rejected: %s", msg)
	}
	return out, nil
}

// CreateOrder places a fully autonomous store order paid via the store-side
// payment token referenced by paymentMethodRef. The bridge computes the
// canonical total from live store prices.
func (c *Client) CreateOrder(ctx context.Context, items []OrderItem, paymentMethodRef, agentID, purchaseID string) (orderID int, status string, total float64, err error) {
	body := map[string]any{
		"items":              items,
		"payment_method_ref": paymentMethodRef,
		"metadata":           map[string]string{"agent_id": agentID, "purchase_id": purchaseID},
	}
	var out struct {
		OrderID  int     `json:"order_id"`
		Status   string  `json:"status"`
		Total    float64 `json:"total"`
		Currency string  `json:"currency"`
		Error    string  `json:"error"`
	}
	if err = c.postJSON(ctx, c.cfg.BaseURL+"/orders", body, &out); err != nil {
		return 0, "", 0, err
	}
	if out.Error != "" || out.OrderID == 0 {
		return 0, "", 0, fmt.Errorf("bridge order rejected: %s", out.Error)
	}
	return out.OrderID, out.Status, out.Total, nil
}

// OrderStatus maps a bridge order to a canonical status
// (pending / processing / completed / failed).
func (c *Client) OrderStatus(ctx context.Context, orderID int) (string, error) {
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := c.getJSON(ctx, fmt.Sprintf("%s/orders/status?order_id=%d", c.cfg.BaseURL, orderID), &out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", fmt.Errorf("bridge order status: %s", out.Error)
	}
	return out.Status, nil
}

// CancelOrder cancels a placed order at the store (used when the live total
// diverges from the quoted total — the reservation is then released).
func (c *Client) CancelOrder(ctx context.Context, orderID int) error {
	var out struct {
		Error string `json:"error"`
	}
	return c.postJSON(ctx, c.cfg.BaseURL+"/orders/cancel", map[string]any{"order_id": orderID}, &out)
}

func (c *Client) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) postJSON(ctx context.Context, url string, payload, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	if c.cfg.APIKey != "" {
		req.Header.Set("X-AgentShopping-Key", c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("bridge http %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}
