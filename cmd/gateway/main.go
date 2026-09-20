// Command gateway starts the AgentShopping central gateway HTTP server.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/cawa0505/agentshopping-gateway/internal/router"
)

func main() {
	addr := os.Getenv("GATEWAY_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("agentshopping-gateway listening on %s", addr)
	if err := http.ListenAndServe(addr, router.New()); err != nil {
		log.Fatal(err)
	}
}
