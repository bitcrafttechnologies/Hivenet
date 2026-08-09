// Package auth is the multi-tenancy seam described in spec §8.
//
// It is a seam, not a feature. v1 ships a no-op implementation that returns the
// single constant owner for every request. A self-hoster who wants real
// multi-user auth implements Authenticator — OIDC, reverse-proxy headers,
// whatever — without touching the reconciler, the driver or the store.
//
// Explicitly not here, and not to be added: session storage, user management,
// password handling, RBAC.
package auth

import (
	"context"
	"net/http"

	"github.com/bitcrafttech/hivenet/internal/topology"
)

// Authenticator resolves the owner a request acts on behalf of. Returning an
// error rejects the request with 401.
type Authenticator interface {
	Authenticate(r *http.Request) (owner string, err error)
}

// NoOp is the v1 default: every request belongs to topology.DefaultOwner.
type NoOp struct{}

// Authenticate always succeeds with the constant single-user owner.
func (NoOp) Authenticate(*http.Request) (string, error) { return topology.DefaultOwner, nil }

type ctxKey struct{}

// Middleware resolves the owner once per request and puts it in the context, so
// handlers below it never call the Authenticator directly.
func Middleware(a Authenticator) func(http.Handler) http.Handler {
	if a == nil {
		a = NoOp{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			owner, err := a.Authenticate(r)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, owner)))
		})
	}
}

// OwnerFrom returns the owner attached by Middleware, falling back to the
// default owner so a handler reached outside the middleware still works.
func OwnerFrom(ctx context.Context) string {
	if owner, ok := ctx.Value(ctxKey{}).(string); ok && owner != "" {
		return owner
	}
	return topology.DefaultOwner
}
