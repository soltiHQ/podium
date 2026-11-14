package middleware

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// ReadOnly marks the context of a GET or a HEAD request, which net/http serves
// with the GET handler, storage.MarkReadOnly: a backend with a read replica, as
// the database mode's, may serve the request's listings, scans and counts from
// it, and so accepts the replica's lag. Every other method passes on unmarked. A
// marked request still reads the primary for its point lookups and its
// transactions, a write it makes on a GET included, so no decision rests on the
// replica. app wires it on the UI and API listener alone.
func ReadOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			r = r.WithContext(storage.MarkReadOnly(r.Context()))
		}
		next.ServeHTTP(w, r)
	})
}
