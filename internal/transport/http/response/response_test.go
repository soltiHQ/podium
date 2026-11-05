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
		{"leadership lost → 503", cluster.ErrLeadershipLost, http.StatusServiceUnavailable},
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

// A write that reached a follower, or that lost leadership while it was
// committing, answers 503, with the leader's HTTP address in X-Leader when the
// Leader middleware told where the leader is. Any other unavailable answer, and
// one with no leader known, sends no hint.
func TestFromErrorNotLeaderCarriesTheLeader(t *testing.T) {
	notLeader := fmt.Errorf("raft: WithTx: %w", cluster.ErrNotLeader)
	leadershipLost := fmt.Errorf("raft: apply: %w", cluster.ErrLeadershipLost)
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
		{"leadership lost, a leader known", leadershipLost, func() string { return "10.0.0.3:8080" }, "10.0.0.3:8080"},
		{"leadership lost, no leader known", leadershipLost, func() string { return "" }, ""},
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

// UnprocessableEntity and NotLeader answer with the message they are given:
// 422, and 503 with the leader's HTTP address that its caller read from the
// Leader middleware in X-Leader, none when it read none. UnavailableMsg is the
// 503 without it.
func TestAnswersWithAMessage(t *testing.T) {
	const msg = "a message of the handler"
	hint := func() string { return "10.0.0.2:8080" }
	notLeader := func(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, msg string) {
		NotLeader(w, r, mode, httpctx.Leader(r.Context()), msg)
	}
	for _, tt := range []struct {
		name   string
		answer func(http.ResponseWriter, *http.Request, httpctx.RenderMode, string)
		leader func() string // what the Leader middleware tells, nil without it
		code   int
		header string // X-Leader
	}{
		{"unprocessable", UnprocessableEntity, hint, http.StatusUnprocessableEntity, ""},
		{"not the leader", notLeader, hint, http.StatusServiceUnavailable, "10.0.0.2:8080"},
		{"not the leader, no leader known", notLeader, nil, http.StatusServiceUnavailable, ""},
		{"unavailable", UnavailableMsg, hint, http.StatusServiceUnavailable, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			if tt.leader != nil {
				r = r.WithContext(httpctx.WithLeader(r.Context(), tt.leader))
			}
			w := httptest.NewRecorder()

			tt.answer(w, r, httpctx.RenderBlock, msg)

			var body errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != tt.code || body.Code != tt.code || body.Message != msg {
				t.Fatalf("HTTP %d %s (%v), want %d with %q", w.Code, w.Body.String(), err, tt.code, msg)
			}
			if got := w.Header().Get(LeaderHeader); got != tt.header {
				t.Fatalf("%s = %q, want %q", LeaderHeader, got, tt.header)
			}
		})
	}
}

// SetLeader is the one place that sets X-Leader, and an empty address, which a
// node that does not know the leader's HTTP address has, sets nothing.
func TestSetLeader(t *testing.T) {
	for _, tt := range []struct{ name, addr, want string }{
		{"an address", "10.0.0.2:8080", "10.0.0.2:8080"},
		{"no address", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			SetLeader(w, tt.addr)
			if _, set := w.Header()[LeaderHeader]; set != (tt.want != "") || w.Header().Get(LeaderHeader) != tt.want {
				t.Fatalf("%s = %q, want %q", LeaderHeader, w.Header().Get(LeaderHeader), tt.want)
			}
		})
	}
}

// StoreFailed answers a write the store failed as Unavailable does, 503 whatever
// the error, which FromError would answer 500 or 404 or 409; only a leadership
// error goes through FromError, so that its 503 carries the leader's address.
func TestStoreFailed(t *testing.T) {
	hint := func() string { return "10.0.0.2:8080" }
	tests := []struct {
		name string
		err  error
		want string // X-Leader
	}{
		{"not the leader", fmt.Errorf("raft: WithTx: %w", cluster.ErrNotLeader), "10.0.0.2:8080"},
		{"leadership lost", fmt.Errorf("raft: apply: %w", cluster.ErrLeadershipLost), "10.0.0.2:8080"},
		{"an unknown error", errors.New("boom"), ""},
		{"an unavailable store", storage.ErrUnavailable, ""},
		{"a conflict, which FromError answers 409", storage.ErrConflict, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r = r.WithContext(httpctx.WithLeader(r.Context(), hint))
			w := httptest.NewRecorder()

			StoreFailed(w, r, httpctx.RenderBlock, tt.err)

			if w.Code != http.StatusServiceUnavailable || w.Header().Get(LeaderHeader) != tt.want {
				t.Fatalf("HTTP %d with %s %q, want 503 with %q", w.Code, LeaderHeader, w.Header().Get(LeaderHeader), tt.want)
			}
		})
	}
}
