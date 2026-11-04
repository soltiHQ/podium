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
	"github.com/soltiHQ/control-plane/internal/cluster"
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
		{"not the leader → 503", cluster.ErrNotLeader, http.StatusServiceUnavailable},
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

// A write that reached a follower answers 503, with the leader's HTTP address in
// X-Leader when the Leader middleware told where the leader is. Any other
// unavailable answer, and one with no leader known, sends no hint.
func TestFromErrorNotLeaderCarriesTheLeader(t *testing.T) {
	notLeader := fmt.Errorf("raft: WithTx: %w", cluster.ErrNotLeader)
	tests := []struct {
		name   string
		err    error
		leader func() string // the hint of the request; nil for none
		want   string
	}{
		{"a follower that knows the leader", notLeader, func() string { return "10.0.0.2:8080" }, "10.0.0.2:8080"},
		{"the sentinel itself", cluster.ErrNotLeader, func() string { return "[fd00::2]:8080" }, "[fd00::2]:8080"},
		{"no leader known", notLeader, func() string { return "" }, ""},
		{"no hint in the request", notLeader, nil, ""},
		{"another unavailable error", storage.ErrUnavailable, func() string { return "10.0.0.2:8080" }, ""},
		{"a canceled request", context.Canceled, func() string { return "10.0.0.2:8080" }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.leader != nil {
				r = r.WithContext(httpctx.WithLeader(r.Context(), tt.leader))
			}
			w := httptest.NewRecorder()

			FromError(w, r, httpctx.RenderBlock, tt.err)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
			if got := w.Header().Get(LeaderHeader); got != tt.want {
				t.Fatalf("%s = %q, want %q", LeaderHeader, got, tt.want)
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
