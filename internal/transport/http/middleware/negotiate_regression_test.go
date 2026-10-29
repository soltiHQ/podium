package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

func TestJSONFetchCanNegotiateStructuredErrorsWithHTMXRedirectHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/api/v1/specs/x", nil)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	Negotiate(responder.NewJSON(), responder.NewHTML())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpctx.Responder(r.Context()).Respond(w, r, 409, &responder.View{Data: map[string]string{"message": "stale version"}})
	})).ServeHTTP(w, r)
	if w.Code != 409 || w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Body.String() != `{"message":"stale version"}` {
		t.Fatalf("unreadable error: %d %s", w.Code, w.Body.String())
	}
}
