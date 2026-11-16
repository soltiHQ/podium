package storage

import "context"

type readOnlyKey struct{}

// MarkReadOnly returns a copy of ctx marked read-only: a backend with a read
// replica may serve the listings, scans and counts of ctx from it, and so
// accepts the replica's lag. The HTTP layer marks the GET requests of the UI
// and the REST API; runners, transactions, point lookups and authentication
// never read a replica, so no decision rests on a lagging copy. A backend
// without a replica ignores the mark.
func MarkReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyKey{}, true)
}

// IsReadOnly reports whether ctx, or a context it derives from, was marked by
// MarkReadOnly.
func IsReadOnly(ctx context.Context) bool {
	marked, _ := ctx.Value(readOnlyKey{}).(bool)
	return marked
}
