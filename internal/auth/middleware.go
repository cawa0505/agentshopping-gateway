// Package auth guards the standard protocol module routes with NexusLedger
// identity + ability checks. The gateway verifies NexusLedger-issued Ed25519
// JWTs and asks NexusLedger whether the agent owns the required ability; it
// never mints tokens or stores abilities itself.
package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/cawa0505/agentshopping-gateway/internal/nexusledger"
)

// ctxKey is the private type for request-scoped claims.
type ctxKey struct{}

// Claims pulls the verified identity claims from a request context, if any.
func Claims(ctx context.Context) (map[string]any, bool) {
	c, ok := ctx.Value(ctxKey{}).(map[string]any)
	return c, ok
}

// Requirement declares what a route needs.
//
// ReadOnly routes (catalog browsing) degrade open: a missing token, or a
// NexusLedger outage, lets the request through anonymously rather than blocking
// browsing. A *present but invalid* token on any route is still rejected only
// when the route is not ReadOnly — ReadOnly browsing must never hard-fail on
// auth. Non-ReadOnly routes require a valid token and, when set, the Ability.
type Requirement struct {
	Ability  string
	ReadOnly bool
}

// Middleware wraps next with the identity/ability policy in req.
func Middleware(nxl *nexusledger.Client, req Requirement, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)

		if token == "" {
			if req.ReadOnly {
				next.ServeHTTP(w, r) // anonymous browsing
				return
			}
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}

		claims, err := nxl.VerifyIdentity(r.Context(), token)
		if err != nil {
			// ponytail: read-only browsing degrades open even if NexusLedger is
			// down or the token is stale — never block a catalog read on auth.
			if req.ReadOnly {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		if req.Ability != "" {
			agent, _ := claims["sub"].(string)
			ok, err := nxl.CheckAbility(r.Context(), agent, req.Ability)
			if err != nil || !ok {
				http.Error(w, "ability "+req.Ability+" required", http.StatusForbidden)
				return
			}
		}

		ctx := context.WithValue(r.Context(), ctxKey{}, map[string]any(claims))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}
