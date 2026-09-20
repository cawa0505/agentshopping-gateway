package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnknownPathReturns404(t *testing.T) {
	srv := httptest.NewServer(New())
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
	srv := httptest.NewServer(New())
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
	srv := httptest.NewServer(New())
	defer srv.Close()

	for _, m := range []string{Catalog, Cart, Checkout, PostOrder} {
		resp, err := http.Post(srv.URL+"/api/mcp/"+m, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		// Registered route reaches a handler (not the 404 fall-through).
		if resp.StatusCode == http.StatusNotFound {
			t.Fatalf("module %s: route not registered", m)
		}
	}
}
