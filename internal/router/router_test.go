package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cawa0505/agentshopping-gateway/internal/nexusledger"
)

// testClient points at an unreachable NexusLedger; these tests only exercise
// health, 404 fall-through, and route registration, none of which reach NXL
// (catalog is anonymous; write routes 401 before any NXL call when no token).
func testClient() *nexusledger.Client {
	return nexusledger.New(nexusledger.Config{BaseURL: "http://127.0.0.1:0", JWKSTTL: time.Second})
}

func TestUnknownPathReturns404(t *testing.T) {
	srv := httptest.NewServer(New(testClient()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/mcp/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path: got %d, want 404", resp.StatusCode)
	}
}

func TestHealthOK(t *testing.T) {
	srv := httptest.NewServer(New(testClient()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: got %d, want 200", resp.StatusCode)
	}
}

func TestKnownModuleRoutes(t *testing.T) {
	srv := httptest.NewServer(New(testClient()))
	defer srv.Close()

	for _, m := range []string{Catalog, Cart, Checkout, PostOrder} {
		resp, err := http.Post(srv.URL+"/api/mcp/"+m, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		// Registered route reaches a handler (auth or placeholder), not the 404 fall-through.
		if resp.StatusCode == http.StatusNotFound {
			t.Fatalf("module %s: route not registered", m)
		}
	}
}
