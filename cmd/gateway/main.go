// Command gateway starts the AgentShopping central gateway HTTP server.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/bridge"
	"github.com/cawa0505/agentshopping-gateway/internal/handlers"
	"github.com/cawa0505/agentshopping-gateway/internal/nexusledger"
	"github.com/cawa0505/agentshopping-gateway/internal/pricing"
	"github.com/cawa0505/agentshopping-gateway/internal/router"
	"github.com/cawa0505/agentshopping-gateway/internal/store"
)

func main() {
	addr := os.Getenv("GATEWAY_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	nxl := nexusledger.New(nexusledger.Config{
		BaseURL: os.Getenv("NXL_BASE_URL"),
		JWKSURL: os.Getenv("NXL_JWKS_URL"),
		SiteID:  os.Getenv("NXL_SITE_ID"),
		JWKSTTL: 5 * time.Minute,
	})

	deps := handlers.Deps{
		Bridge: bridge.New(bridge.Config{
			BaseURL: os.Getenv("BRIDGE_BASE_URL"),
			APIKey:  os.Getenv("BRIDGE_API_KEY"),
		}),
		Pricing: pricing.New(nxl),
	}

	// Delegated purchasing (closed-source core): SQLite-backed authorization
	// service + WooCommerce merchant adapter over the store bridge.
	if dsn := os.Getenv("PURCHASING_DB"); dsn != "" {
		st, err := store.New(dsn)
		if err != nil {
			log.Fatal(err)
		}
		defer st.Close()
		adapter := bridge.NewWooCommerceAdapter(deps.Bridge)
		deps.Purchasing = store.NewService(st, adapter, time.Now)
		// Reconcile sweep: settle EXECUTING purchases whose merchant order
		// status has advanced (paid → CAPTURED, failed → RELEASED).
		go func() {
			for range time.Tick(time.Minute) {
				if _, err := deps.Purchasing.ReconcileExecuting(context.Background()); err != nil {
					log.Printf("purchasing reconcile: %v", err)
				}
			}
		}()
	}

	log.Printf("agentshopping-gateway listening on %s", addr)
	if err := http.ListenAndServe(addr, router.New(nxl, deps)); err != nil {
		log.Fatal(err)
	}
}
