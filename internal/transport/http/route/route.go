// Package route provides HTTP routing helpers used across UI and API handlers.
//
// Composition:
//   - [BaseMW], [PermMW]     - middleware signatures.
//   - [Chain]                - wrap a handler with middleware left-to-right.
//   - [Handle], [HandleFunc] - register a pattern on ServeMux with a chain.
//
// REST dispatch (see dispatch.go):
//   - [CollectionHandler], [EntityHandler] - handler types with render mode.
//   - [Endpoint], [Subroute]               - method/permission/handler triples.
//   - [Guard]                              - apply permission middleware.
//   - [Resource]                           - dispatch /collection by method.
//   - [Router]                             - dispatch /collection/{id}[/action].
package route

import (
	"net/http"

	"github.com/soltiHQ/control-plane/domain/kind"
)

// BaseMW is a standard HTTP middleware signature.
type BaseMW func(http.Handler) http.Handler

// PermMW creates a middleware that guards a route with the given permission.
type PermMW func(kind.Permission) BaseMW

// Chain wraps a handler with middleware applied left-to-right:
//
//	Chain(h, a, b) => a(b(h))
func Chain(h http.Handler, mws ...BaseMW) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] == nil {
			continue
		}
		h = mws[i](h)
	}
	return h
}

// Handle registers a pattern with optional middleware.
func Handle(mux *http.ServeMux, pattern string, h http.Handler, mws ...BaseMW) {
	mux.Handle(pattern, Chain(h, mws...))
}

// HandleFunc registers a handler func with optional middleware.
func HandleFunc(mux *http.ServeMux, pattern string, fn http.HandlerFunc, mws ...BaseMW) {
	mux.Handle(pattern, Chain(fn, mws...))
}
