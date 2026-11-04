package raft

import (
	"context"
	"crypto/sha3"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/httpcookie"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
	grpcstatus "github.com/soltiHQ/control-plane/internal/transport/grpc/status"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

// follower returns a running node that is not leader, once it knows its leader.
func (c *loopbackCluster) follower(t *testing.T, leader *loopbackNode) *loopbackNode {
	t.Helper()
	for _, n := range c.nodes {
		if n == leader || n.profile == nil {
			continue
		}
		waitFor(t, 5*time.Second, n.id+" knows the leader "+leader.id, func() bool {
			return n.profile.leadership.CurrentLeader() == leader.addr
		})
		return n
	}
	t.Fatal("no follower")
	return nil
}

// A write on a follower fails with ErrNotLeader and changes nothing, whether it
// is a single operation or a transaction, which fails before its function runs.
// Both transports answer it as unavailable: gRPC with Unavailable, HTTP with 503
// and the leader's HTTP address in X-Leader, the host the leader advertises for
// raft and the port this node serves HTTP on.
func TestFollowerWriteIsUnavailableWithLeaderHint(t *testing.T) {
	const httpPort = 18080
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	leader := c.leader(t, 10*time.Second)
	follower := c.follower(t, leader)
	store := follower.profile.Store()
	wantLeader := net.JoinHostPort("127.0.0.1", strconv.Itoa(httpPort))

	agent, err := model.NewAgent("a1", "agent-1", "http://a1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		write func(ran *bool) error
	}{
		{"single operation", func(*bool) error { return store.UpsertAgent(ctx, agent) }},
		{"transaction", func(ran *bool) error {
			return store.WithTx(ctx, func(tx storage.Storage) error {
				*ran = true
				return tx.UpsertAgent(ctx, agent)
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran bool
			err := tc.write(&ran)
			if !errors.Is(err, ErrNotLeader) {
				t.Fatalf("write on a follower = %v, want %v", err, ErrNotLeader)
			}
			if ran {
				t.Fatal("a transaction on a follower ran its function")
			}
			if kind := errkind.Classify(err); kind != errkind.Unavailable {
				t.Fatalf("errkind.Classify(%v) = %v, want Unavailable", err, kind)
			}
			if code := grpcstatus.FromError(ctx, err).Code(); code != codes.Unavailable {
				t.Fatalf("gRPC status of %v = %v, want Unavailable", err, code)
			}

			// A read that writes, such as a refresh with rotation, reaches the
			// follower's handlers: the Leader middleware passes reads through.
			handler := middleware.Leader(follower.profile.Leadership(), middleware.LeaderOptions{ForwardPort: httpPort})(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					response.FromError(w, r, httpctx.RenderBlock, tc.write(new(bool)))
				}))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("HTTP status %d, want 503", w.Code)
			}
			if got := w.Header().Get("X-Leader"); got != wantLeader {
				t.Fatalf("X-Leader = %q, want %q", got, wantLeader)
			}
		})
	}
	if _, err := follower.profile.inner.GetAgent(ctx, "a1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the follower holds the agent of a refused write: %v", err)
	}
}

// hashicorp/raft's own ErrNotLeader, wrapped or not, becomes ErrNotLeader, and
// every other error, such as a leadership lost while committing, stays.
func TestLeaderError(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"nil", nil, nil},
		{"hashicorp's ErrNotLeader", hraft.ErrNotLeader, ErrNotLeader},
		{"wrapped", fmt.Errorf("barrier: %w", hraft.ErrNotLeader), ErrNotLeader},
		{"leadership lost", hraft.ErrLeadershipLost, hraft.ErrLeadershipLost},
		{"another error", boom, boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := leaderError(tc.err); got != tc.want {
				t.Fatalf("leaderError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// With auth.rotate_refresh a browser whose access token has expired is refreshed
// by the Auth middleware, on a GET as well, and the rotation of its refresh
// token is a write. On a follower the refresh fails with ErrNotLeader before it
// consumes the token: the request answers 503 with the leader's address in
// X-Leader and sets no cookie, the follower logs a WARN, and the same cookies
// refresh at the leader.
func TestRotatingRefreshOnAFollowerAsksForTheLeader(t *testing.T) {
	const (
		httpPort = 18080
		refresh  = "test-only-refresh-token"
	)
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	leader := c.leader(t, 10*time.Second)
	follower := c.follower(t, leader)
	user, err := model.NewUser("refresh-user", "refresh-subject")
	if err != nil {
		t.Fatal(err)
	}
	user.Enable()
	user.SetPermissions([]string{string(enum.SpecsGet)})
	hash := sha3.Sum256([]byte(refresh))
	sess, err := model.NewSession("refresh-session", user.ID(), "credential", enum.Password, hash[:], time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c.write(t, 10*time.Second, func(s storage.Storage) error {
		return s.WithTx(ctx, func(tx storage.Storage) error {
			if err := tx.UpsertUser(ctx, user); err != nil {
				return err
			}
			return tx.CreateSession(ctx, sess)
		})
	})
	waitFor(t, 5*time.Second, follower.id+" applies the session", func() bool {
		_, err := follower.profile.inner.GetSession(ctx, sess.ID())
		return err == nil
	})

	// chain is the Auth middleware over a node's own store, behind the Leader middleware.
	secret, clock := []byte("test-only-raft-refresh-signing-key"), token.RealClock()
	chain := func(n *loopbackNode, logs *logBuffer) http.Handler {
		store := n.profile.Store()
		svc := session.New(store, jwt.NewHSIssuer(secret, clock), clock, session.Config{AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, Issuer: "raft-test", Audience: "raft-test", RotateRefresh: true}, rbac.NewResolver(store), nil)
		auth := middleware.Auth(jwt.NewHSVerifier("raft-test", "raft-test", secret, clock), svc, func(*http.Request) bool { return false }, zerolog.New(logs))
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		return middleware.Leader(n.profile.Leadership(), middleware.LeaderOptions{ForwardPort: httpPort})(auth(next))
	}
	get := func(h http.Handler) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/ui/specs", nil)
		r.AddCookie(&http.Cookie{Name: httpcookie.NameRefreshToken, Value: refresh})
		r.AddCookie(&http.Cookie{Name: httpcookie.NameSessionID, Value: sess.ID()})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	logs := &logBuffer{}
	w := get(chain(follower, logs))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("X-Leader") != net.JoinHostPort("127.0.0.1", strconv.Itoa(httpPort)) || len(w.Result().Cookies()) != 0 {
		t.Fatalf("the follower answered %d with X-Leader %q and %d cookies, want 503 with the leader's address and none", w.Code, w.Header().Get("X-Leader"), len(w.Result().Cookies()))
	}
	lines := logLines(t, logs)
	if len(lines) != 1 {
		t.Fatalf("the follower logged %v, want one WARN about auth.rotate_refresh", lines)
	}
	if message, _ := lines[0]["message"].(string); lines[0]["level"] != "warn" || !strings.Contains(message, "auth.rotate_refresh") {
		t.Fatalf("the follower logged %v, want one WARN about auth.rotate_refresh", lines)
	}
	if w := get(chain(leader, &logBuffer{})); w.Code != http.StatusNoContent || len(w.Result().Cookies()) != 3 {
		t.Fatalf("the leader answered %d with %d cookies, want 204 with the three auth cookies", w.Code, len(w.Result().Cookies()))
	}
}
