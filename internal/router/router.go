// Package router wires the AgentShopping standard protocol modules onto a
// stdlib http.ServeMux. Auth/ability middleware and platform bridges attach
// to these routes in later tasks.
package router

import (
	"net/http"
)

// Module names of the AgentShopping standard protocol.
const (
	Catalog   = "catalog"
	Cart      = "cart"
	Checkout  = "checkout"
	PostOrder = "post-order"
)

// New returns the gateway HTTP handler. Unregistered paths fall through to the
// ServeMux default 404. Real module handlers are injected in later tasks; the
// skeleton mounts a liveness probe plus placeholder module endpoints so routing
// and 404 behavior are testable now.
func New() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// ponytail: placeholder module handlers — real bridge dispatch lands in task 3.
	for _, m := range []string{Catalog, Cart, Checkout, PostOrder} {
		mux.HandleFunc("POST /api/mcp/"+m, notImplemented)
	}

	return mux
}

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
