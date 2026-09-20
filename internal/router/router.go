// Package router wires the AgentShopping standard protocol modules onto a
// stdlib http.ServeMux, guarding module routes with NexusLedger auth and
// dispatching to the store bridge + ledger pricing handlers.
package router

import (
	"net/http"

	"github.com/cawa0505/agentshopping-gateway/internal/auth"
	"github.com/cawa0505/agentshopping-gateway/internal/handlers"
	"github.com/cawa0505/agentshopping-gateway/internal/nexusledger"
)

// Module names of the AgentShopping standard protocol.
const (
	Catalog   = "catalog"
	Cart      = "cart"
	Checkout  = "checkout"
	PostOrder = "post-order"
)

// moduleReq maps each standard module to its auth requirement. Catalog is
// read-only (anonymous browsing allowed); cart/checkout/post-order require a
// valid agent identity plus the matching ability.
var moduleReq = map[string]auth.Requirement{
	Catalog:   {ReadOnly: true},
	Cart:      {Ability: "cart:write"},
	Checkout:  {Ability: "checkout:pay"},
	PostOrder: {Ability: "post-order:read"},
}

// New returns the gateway HTTP handler. Health and unknown paths bypass auth;
// module routes are wrapped with NexusLedger identity/ability middleware and
// dispatch to the bridge + pricing handlers.
func New(nxl *nexusledger.Client, deps handlers.Deps) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	handler := map[string]http.HandlerFunc{
		Catalog:   deps.Catalog,
		Cart:      deps.Cart,
		Checkout:  deps.Checkout,
		PostOrder: deps.PostOrder,
	}
	for _, m := range []string{Catalog, Cart, Checkout, PostOrder} {
		mux.Handle("POST /api/mcp/"+m, auth.Middleware(nxl, moduleReq[m], handler[m]))
	}

	return mux
}
