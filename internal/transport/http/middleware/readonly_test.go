package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// TestReadOnlyMarksGetOnly checks that ReadOnly marks the context of a GET and
// a HEAD request, which net/http serves with the GET handler, read-only, and
// passes every other method on unmarked.
func TestReadOnlyMarksGetOnly(t *testing.T) {
	for _, tc := range []struct {
		method string
		marked bool
	}{
		{http.MethodGet, true},
		{http.MethodHead, true},
		{http.MethodPost, false},
		{http.MethodPut, false},
		{http.MethodPatch, false},
		{http.MethodDelete, false},
		{http.MethodOptions, false},
		{http.MethodConnect, false},
		{http.MethodTrace, false},
		{"PROPFIND", false},
	} {
		var (
			reached bool
			marked  bool
		)
		h := ReadOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached, marked = true, storage.IsReadOnly(r.Context())
			w.WriteHeader(http.StatusNoContent)
		}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, "/api/v1/agents", nil))
		switch {
		case !reached || w.Code != http.StatusNoContent:
			t.Errorf("%s: the handler ran %v, HTTP %d; want it to run and answer", tc.method, reached, w.Code)
		case marked != tc.marked:
			t.Errorf("%s: marked read-only %v, want %v", tc.method, marked, tc.marked)
		}
	}
}
