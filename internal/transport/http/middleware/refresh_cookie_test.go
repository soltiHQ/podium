package middleware

import (
	"context"
	"crypto/sha3"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/httpcookie"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

type cookieRefreshReadBarrier struct {
	storage.Storage
	reads         atomic.Int32
	both, release chan struct{}
}

func (s *cookieRefreshReadBarrier) GetSession(ctx context.Context, id string) (*model.Session, error) {
	value, err := s.Storage.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	n := s.reads.Add(1)
	if n == 2 {
		close(s.both)
	}
	if n <= 2 {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return value, nil
}

func cookieRefreshFixture(t *testing.T) (*inmemory.Store, *model.Session, []*http.Cookie) {
	t.Helper()
	st := inmemory.New()
	ctx := context.Background()
	user, err := model.NewUser("cookie-user", "cookie-subject")
	if err != nil {
		t.Fatal(err)
	}
	user.Enable()
	user.SetPermissions([]string{string(enum.SpecsGet)})
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	raw := "test-only-original-refresh"
	hash := sha3.Sum256([]byte(raw))
	sess, err := model.NewSession("cookie-session", user.ID(), "credential", enum.Password, hash[:], time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return st, sess, []*http.Cookie{{Name: httpcookie.NameRefreshToken, Value: raw}, {Name: httpcookie.NameSessionID, Value: sess.ID()}}
}

func cookieRefreshHandler(st storage.Storage, resolver session.RBACResolver, next http.Handler) http.Handler {
	clk := token.RealClock()
	secret := []byte("test-only-local-refresh-cookie-signing-key")
	svc := session.New(st, jwt.NewHSIssuer(secret, clk), clk, session.Config{AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, Issuer: "cookie-test", Audience: "cookie-client", RotateRefresh: true}, resolver, nil)
	return Auth(jwt.NewHSVerifier("cookie-test", "cookie-client", secret, clk), svc)(RequirePermission(enum.SpecsGet)(next))
}

func cookieRefreshRequest(ctx context.Context, target string, cookies []*http.Cookie) *http.Request {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	for _, c := range cookies {
		request.AddCookie(c)
	}
	return request
}

func TestRefreshLosingConcurrentResponseCannotClearWinningCookies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, _, initial := cookieRefreshFixture(t)
	barrier := &cookieRefreshReadBarrier{Storage: st, both: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(barrier.release) }) }
	defer release()
	server := httptest.NewServer(cookieRefreshHandler(barrier, rbac.NewResolver(st), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })))
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	type result struct {
		response *http.Response
		err      error
	}
	replies := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			response, err := client.Do(cookieRefreshRequest(ctx, server.URL, initial))
			replies <- result{response, err}
		}()
	}
	select {
	case <-barrier.both:
		release()
	case <-ctx.Done():
		t.Fatal("concurrent requests did not both read old session")
	}
	var winner, loser *http.Response
	for i := 0; i < 2; i++ {
		r := <-replies
		if r.err != nil {
			t.Fatal(r.err)
		}
		_, _ = io.Copy(io.Discard, r.response.Body)
		_ = r.response.Body.Close()
		switch r.response.StatusCode {
		case 204:
			winner = r.response
		case 401:
			loser = r.response
		default:
			t.Fatalf("unexpected concurrent status:%d", r.response.StatusCode)
		}
	}
	if winner == nil || loser == nil {
		t.Fatal("rotation did not have exactly one winner and one loser")
	}
	if len(winner.Cookies()) != 3 {
		t.Fatalf("winning response has%d cookies", len(winner.Cookies()))
	}
	if len(loser.Cookies()) != 0 {
		t.Errorf("losing response overwrites winner with%d auth cookies", len(loser.Cookies()))
	}
	jar, _ := cookiejar.New(nil)
	target, _ := url.Parse(server.URL)
	// Apply the losing response LAST, as a browser receiving it after success.
	jar.SetCookies(target, winner.Cookies())
	jar.SetCookies(target, loser.Cookies())
	client.Jar = jar
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	continued, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = continued.Body.Close() }()
	if continued.StatusCode != 204 {
		t.Fatalf("late losing response destroyed winning authentication:HTTP%d", continued.StatusCode)
	}
	replayClient := &http.Client{Timeout: 3 * time.Second}
	defer replayClient.CloseIdleConnections()
	replay, err := replayClient.Do(cookieRefreshRequest(ctx, server.URL, initial))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replay.Body.Close() }()
	if replay.StatusCode != 401 {
		t.Fatalf("consumed old refresh replay statusHTTP%d", replay.StatusCode)
	}
}

type cookieRefreshPermissionGate struct {
	inner            session.RBACResolver
	calls            atomic.Int32
	entered, release chan struct{}
}

func (r *cookieRefreshPermissionGate) ResolveUserPermissions(ctx context.Context, u *model.User) ([]enum.Permission, error) {
	permissions, err := r.inner.ResolveUserPermissions(ctx, u)
	if r.calls.Add(1) == 1 {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return permissions, err
}

func TestRefreshCurrentRequestRechecksChangedPermissions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, sess, cookies := cookieRefreshFixture(t)
	gate := &cookieRefreshPermissionGate{inner: rbac.NewResolver(st), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
	defer release()
	var handlerCalls atomic.Int32
	server := httptest.NewServer(cookieRefreshHandler(st, gate, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { handlerCalls.Add(1); w.WriteHeader(204) })))
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	replies := make(chan *http.Response, 1)
	failures := make(chan error, 1)
	go func() {
		response, err := client.Do(cookieRefreshRequest(ctx, server.URL, cookies))
		if err != nil {
			failures <- err
			return
		}
		replies <- response
	}()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("request did not reach permission resolution")
	}
	user, err := st.GetUser(ctx, sess.UserID())
	if err != nil {
		t.Fatal(err)
	}
	user.SetPermissions([]string{string(enum.AgentsGet)})
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case response := <-replies:
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != 403 {
			t.Fatalf("permission removed during refresh still authorized current request:HTTP%d", response.StatusCode)
		}
	case err := <-failures:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("permission request did not finish")
	}
	if handlerCalls.Load() != 0 {
		t.Fatal("stale permission reached protected handler")
	}
}
