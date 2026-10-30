package response

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

func TestFromError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"not found → 404", storage.ErrNotFound, http.StatusNotFound},
		{"already exists → 409", storage.ErrAlreadyExists, http.StatusConflict},
		{"conflict → 409", storage.ErrConflict, http.StatusConflict},
		{"invalid argument → 400", storage.ErrInvalidArgument, http.StatusBadRequest},
		{"unauthorized → 403", auth.ErrUnauthorized, http.StatusForbidden},
		{"invalid token → 401", auth.ErrInvalidToken, http.StatusUnauthorized},
		{"user disabled → 409", auth.ErrUserDisabled, http.StatusConflict},
		{"canceled → 503", context.Canceled, http.StatusServiceUnavailable},
		{"unknown → 500", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			w := httptest.NewRecorder()

			FromError(w, r, httpctx.RenderBlock, tt.err)

			if w.Code != tt.want {
				t.Fatalf("FromError(%v) status = %d, want %d", tt.err, w.Code, tt.want)
			}
		})
	}
}

func TestPreconditionFailedNegotiates(t *testing.T) {
	const msg = "Placement changed since it was previewed. Review it again."
	previous := ErrorPageRenderer
	t.Cleanup(func() { ErrorPageRenderer = previous })
	ErrorPageRenderer = func(code int, title, message string) templ.Component {
		return templ.Raw(fmt.Sprintf("<h1>%d %s</h1><p>%s</p>", code, title, message))
	}

	tests := []struct {
		name      string
		responder responder.Responder
		mode      httpctx.RenderMode
		wantType  string
		wantHTML  string
	}{
		{"JSON", responder.NewJSON(), httpctx.RenderBlock, "application/json", ""},
		{"HTML page", responder.NewHTML(), httpctx.RenderPage, "text/html", "<h1>412 Precondition failed</h1><p>" + msg + "</p>"},
		{"HTML block", responder.NewHTML(), httpctx.RenderBlock, "text/html", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r = r.WithContext(httpctx.WithResponder(r.Context(), tt.responder))
			w := httptest.NewRecorder()

			PreconditionFailed(w, r, tt.mode, msg)

			if w.Code != http.StatusPreconditionFailed || !strings.HasPrefix(w.Header().Get("Content-Type"), tt.wantType) {
				t.Fatalf("got %d %q, want 412 %q", w.Code, w.Header().Get("Content-Type"), tt.wantType)
			}
			if tt.wantType == "application/json" {
				var body errorBody
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != http.StatusPreconditionFailed || body.Message != msg {
					t.Fatalf("JSON body %s (%v)", w.Body.String(), err)
				}
			} else if w.Body.String() != tt.wantHTML {
				t.Fatalf("HTML body %q, want %q", w.Body.String(), tt.wantHTML)
			}
		})
	}
}
