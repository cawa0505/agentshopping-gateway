// Command gateway starts the AgentShopping central gateway HTTP server.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/bridge"
	"github.com/cawa0505/agentshopping-gateway/internal/handlers"
	"github.com/cawa0505/agentshopping-gateway/internal/nexusledger"
	"github.com/cawa0505/agentshopping-gateway/internal/pricing"
	"github.com/cawa0505/agentshopping-gateway/internal/router"
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

	log.Printf("agentshopping-gateway listening on %s", addr)
	if err := http.ListenAndServe(addr, router.New(nxl, deps)); err != nil {
		log.Fatal(err)
	}
}
