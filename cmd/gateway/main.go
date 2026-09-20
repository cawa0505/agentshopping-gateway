// Command gateway starts the AgentShopping central gateway HTTP server.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/nexusledger"
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
		JWKSTTL: 5 * time.Minute,
	})

	log.Printf("agentshopping-gateway listening on %s", addr)
	if err := http.ListenAndServe(addr, router.New(nxl)); err != nil {
		log.Fatal(err)
	}
}
