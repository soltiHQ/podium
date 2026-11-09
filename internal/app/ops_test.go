package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/buildinfo"
	"github.com/soltiHQ/control-plane/internal/config"
)

// runningApp is an app that New built and Run serves, as main runs it: standalone
// with ephemeral state, every listener on a loopback port the kernel picks.
type runningApp struct {
	app    *App
	cfg    config.Config
	logs   *syncBuffer
	cancel context.CancelFunc // the shutdown signal
	done   chan struct{}      // closed once Run has returned
	err    error              // what Run returned, set before done closes
}

// startApp builds the app of a default configuration that configure changes, and
// before it runs calls before, which may set the seams of its readiness. The app
// stops when the test ends.
func startApp(t *testing.T, configure func(*config.Config), before func(*App)) *runningApp {
	t.Helper()
	cfg := config.Default()
	cfg.Cluster.Standalone.Ephemeral = true
	cfg.Auth.JWTSecret = strings.Repeat("ops-listener-test-secret", 2)
	cfg.Auth = cfg.Auth.WithDefaults()
	cfg.HTTP.Addr, cfg.HTTPDiscovery.Addr, cfg.GRPC.Addr, cfg.Ops.Addr = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
	if configure != nil {
		configure(&cfg)
	}
	logs := &syncBuffer{}
	a, err := New(context.Background(), cfg, zerolog.New(logs), WithPasswordCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	if before != nil {
		before(a)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &runningApp{app: a, cfg: cfg, logs: logs, cancel: cancel, done: make(chan struct{})}
	go func() {
		r.err = a.Run(ctx)
		close(r.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(20 * time.Second):
			t.Error("Run did not return within 20s of the shutdown signal")
		}
		if err := a.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return r
}

// gateDelay replaces the shutdown delay of a, which a test would otherwise wait
// out, with a wait that ends when the returned function is called.
func gateDelay(a *App) (release func()) {
	gate := make(chan struct{})
	a.sleep = func(ctx context.Context, _ time.Duration) {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	return sync.OnceFunc(func() { close(gate) })
}

// get returns the status and the body of GET http://addr+path with header, or
// 0 and the error when the request fails.
func get(addr, path string, header http.Header) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return 0, "", err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	// A redirect is an answer: the UI sends a request without credentials to the login page.
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// opsAnswers waits until GET path of the ops listener at addr answers status with
// body, and fails the test when it does not within 10s.
func opsAnswers(t *testing.T, addr, path string, status int, body string) {
	t.Helper()
	var gotStatus int
	var gotBody string
	var gotErr error
	waitFor(t, 10*time.Second, "ops "+path+" answers "+body, func() bool {
		gotStatus, gotBody, gotErr = get(addr, path, nil)
		return gotErr == nil && gotStatus == status && strings.TrimSpace(gotBody) == body
	})
}

// bearerFor stores an enabled user with permissions and a session in the store of
// a, and returns a bearer token of that session.
func bearerFor(t *testing.T, a *App, cfg config.Config, userID string, permissions ...enum.Permission) string {
	t.Helper()
	ctx := context.Background()
	user, err := model.NewUser(userID, userID)
	if err != nil {
		t.Fatal(err)
	}
	user.Enable()
	perms := make([]string, 0, len(permissions))
	for _, p := range permissions {
		perms = append(perms, string(p))
	}
	user.SetPermissions(perms)
	if err = a.store.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	session, err := model.NewSession("session-"+userID, userID, "credential", enum.Password, make([]byte, 32), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	value, err := jwt.NewHSIssuer([]byte(cfg.Auth.JWTSecret), a.authModel.Clock).Issue(ctx, &identity.Identity{
		UserID: userID, Subject: userID, SessionID: session.ID(), TokenID: "token-" + userID, Issuer: cfg.Auth.Issuer,
		Audience: []string{cfg.Auth.Audience}, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// A node is not ready while its durable store has failed, and not ready once the
// shutdown starts, which it reports while the UI listener still answers, for the
// shutdown delay. The failure is injected at the seam of the readiness; the
// store's own failure is the durable package's test.
func TestReadinessFollowsStoreAndShutdown(t *testing.T) {
	var (
		broken  atomic.Bool
		release func()
	)
	r := startApp(t, func(c *config.Config) { c.Server.ShutdownDelay = time.Hour }, func(a *App) {
		release = gateDelay(a)
		a.ready.failure = func() error {
			if broken.Load() {
				return errors.New("injected durable failure")
			}
			return nil
		}
	})
	ops, ui := r.app.ops.Addr, r.app.ui.Addr
	waitFor(t, 10*time.Second, "the listeners bind", func() bool { return ops() != "" && ui() != "" })
	opsAnswers(t, ops(), "/readyz", 200, "ok")

	broken.Store(true)
	opsAnswers(t, ops(), "/readyz", 503, "storage failed")
	opsAnswers(t, ops(), "/livez", 200, "ok") // a failed store is not a dead process
	broken.Store(false)
	opsAnswers(t, ops(), "/readyz", 200, "ok")

	r.cancel()
	opsAnswers(t, ops(), "/readyz", 503, "shutting down")
	if status, _, err := get(ui(), "/login", nil); err != nil || status != http.StatusOK {
		t.Fatalf("the UI listener during the shutdown delay: %d, %v; want it to answer", status, err)
	}
	select {
	case <-r.done:
		t.Fatal("Run returned during the shutdown delay")
	default:
	}
	release()
	<-r.done
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Run: %v, want context.Canceled", r.err)
	}
	if _, _, err := get(ops(), "/livez", nil); err == nil {
		t.Fatal("the ops listener answered after Run returned")
	}
}

// The reasons of readiness, first that applies: shutting down, then starting, a
// failed store, and for raft a missing leader.
func TestReadinessReasonOrder(t *testing.T) {
	failed := func() error { return errors.New("failed") }
	for _, tc := range []struct {
		name         string
		bound, going bool
		failure      func() error
		leader       func() string
		want         string
	}{
		{"ready", true, false, nil, nil, ""},
		{"starting", false, false, nil, nil, reasonStarting},
		{"failed store", true, false, failed, nil, reasonStorage},
		{"no leader", true, false, nil, func() string { return "" }, reasonNoLeader},
		{"a leader", true, false, nil, func() string { return "n1" }, ""},
		{"shutting down before all", false, true, failed, func() string { return "" }, reasonShuttingDown},
		{"starting before the store", false, false, failed, func() string { return "" }, reasonStarting},
		{"the store before the leader", true, false, failed, func() string { return "" }, reasonStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &readiness{failure: tc.failure, leader: tc.leader}
			r.bound.Store(tc.bound)
			r.shuttingDown.Store(tc.going)
			if got := r.reason(); got != tc.want {
				t.Fatalf("reason %q, want %q", got, tc.want)
			}
		})
	}
}

// With the three-node helper only the leader answers /leaderz with 200; the
// followers say "not leader", and every node that knows a leader is ready.
func TestLeaderProbeOnRaft(t *testing.T) {
	nodes := clusterNodes(t, "n1", "n2", "n3")
	for _, n := range nodes {
		n.start(t)
	}
	for _, n := range nodes {
		waitFor(t, 10*time.Second, n.id+" binds its ops listener", func() bool { return n.app.ops.Addr() != "" })
		opsAnswers(t, n.app.ops.Addr(), "/readyz", 200, "ok")
	}
	var leader string
	waitFor(t, 10*time.Second, "exactly one node answers /leaderz with 200", func() bool {
		var leaders []string
		for _, n := range nodes {
			if status, _, err := get(n.app.ops.Addr(), "/leaderz", nil); err == nil && status == http.StatusOK {
				leaders = append(leaders, n.id)
			}
		}
		if len(leaders) == 1 {
			leader = leaders[0]
		}
		return len(leaders) == 1
	})
	for _, n := range nodes {
		if n.id == leader {
			if !n.app.leadership.AmLeader() {
				t.Fatalf("%s answered /leaderz with 200 and is not the leader", n.id)
			}
			continue
		}
		opsAnswers(t, n.app.ops.Addr(), "/leaderz", 503, "not leader")
		if n.app.leadership.AmLeader() {
			t.Fatalf("%s leads, and says it does not", n.id)
		}
	}
}

// The ops routes answer without credentials, over GET and HEAD, and set no
// cookie; the main listener does not serve them, where an unknown path sends a
// request without credentials to the login page.
func TestOpsEndpointsNeedNoAuth(t *testing.T) {
	r := startApp(t, nil, nil)
	ops, ui := r.app.ops.Addr, r.app.ui.Addr
	waitFor(t, 10*time.Second, "the listeners bind", func() bool { return ops() != "" && ui() != "" })
	opsAnswers(t, ops(), "/readyz", 200, "ok")

	client := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/livez", "/readyz", "/leaderz", "/version"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req, err := http.NewRequest(method, "http://"+ops()+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || len(resp.Header.Values("Set-Cookie")) != 0 {
				t.Fatalf("%s %s: %d with cookies %v, want 200 and none", method, path, resp.StatusCode, resp.Header.Values("Set-Cookie"))
			}
			if method == http.MethodHead && len(body) != 0 {
				t.Fatalf("HEAD %s answered a body", path)
			}
		}
	}
	_, body, err := get(ops(), "/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	var info buildinfo.Info
	if err = json.Unmarshal([]byte(body), &info); err != nil || info != buildinfo.Read() {
		t.Fatalf("/version: %q, %v; want %+v", body, err, buildinfo.Read())
	}
	if status, _, err := get(ui(), "/livez", nil); err != nil || status != http.StatusFound {
		t.Fatalf("the main listener's /livez: %d, %v; want a redirect to the login page: the route is not served there", status, err)
	}
}

// An empty ops.addr runs no ops listener.
func TestOpsListenerDisabledWhenAddrEmpty(t *testing.T) {
	r := startApp(t, func(c *config.Config) { c.Ops.Addr = "" }, nil)
	if r.app.ops != nil {
		t.Fatal("an ops runner exists with ops.addr empty")
	}
	waitFor(t, 10*time.Second, "the other listeners bind", func() bool { return r.app.ui.Addr() != "" && r.app.grpc.Addr() != "" })
	waitFor(t, 10*time.Second, "the app is ready", func() bool { return r.app.ready.reason() == "" })
	node := &clusterNode{id: "standalone", logs: r.logs}
	if got := node.logged(t, map[string]any{"runner": "ops"}); len(got) != 0 {
		t.Fatalf("lines of an ops runner with ops.addr empty: %v", got)
	}
}

// The gRPC health service reports the overall service and the discovery service
// as SERVING while the node is ready, and NOT_SERVING when it is not, without
// authentication: a failed store, and the shutdown, which holds for the delay.
func TestGRPCHealthFollowsReadiness(t *testing.T) {
	var (
		broken  atomic.Bool
		release func()
	)
	r := startApp(t, func(c *config.Config) { c.Server.ShutdownDelay = time.Hour }, func(a *App) {
		release = gateDelay(a)
		a.ready.failure = func() error {
			if broken.Load() {
				return errors.New("injected durable failure")
			}
			return nil
		}
	})
	waitFor(t, 10*time.Second, "the listeners bind", func() bool { return r.app.grpc.Addr() != "" })
	conn, err := grpc.NewClient(r.app.grpc.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client := healthpb.NewHealthClient(conn)
	services := []string{"", discoverv1.DiscoverService_ServiceDesc.ServiceName}
	expect := func(want healthpb.HealthCheckResponse_ServingStatus) {
		t.Helper()
		for _, service := range services {
			waitFor(t, 10*time.Second, "health of "+service+" is "+want.String(), func() bool {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{Service: service})
				return err == nil && resp.GetStatus() == want
			})
		}
	}
	expect(healthpb.HealthCheckResponse_SERVING)
	broken.Store(true)
	expect(healthpb.HealthCheckResponse_NOT_SERVING)
	broken.Store(false)
	expect(healthpb.HealthCheckResponse_SERVING)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err = client.Check(ctx, &healthpb.HealthCheckRequest{Service: "unknown.Service"}); status.Code(err) != codes.NotFound {
		t.Fatalf("an unknown service: %v, want NotFound", err)
	}
	r.cancel()
	expect(healthpb.HealthCheckResponse_NOT_SERVING)
	release()
}

// Run waits the configured shutdown delay, on a real timer, between the signal
// and the end of its listeners.
func TestShutdownDelayIsWaitedOut(t *testing.T) {
	const delay = 300 * time.Millisecond
	r := startApp(t, func(c *config.Config) { c.Server.ShutdownDelay = delay }, nil)
	if r.app.shutdownDelay != delay {
		t.Fatalf("the app's delay is %v, want server.shutdown_delay, %v", r.app.shutdownDelay, delay)
	}
	waitFor(t, 10*time.Second, "the app is ready", func() bool { return r.app.ready.reason() == "" })
	begin := time.Now()
	r.cancel()
	<-r.done
	if took := time.Since(begin); took < delay {
		t.Fatalf("Run returned %v after the signal, before the shutdown delay of %v", took, delay)
	}
}

// A request in flight when the shutdown starts finishes with its answer, while
// the event stream and a task log stream end as the listeners stop accepting. The
// slow request is a task listing, which waits on a fake agent.
func TestInFlightRequestFinishesAtShutdown(t *testing.T) {
	var (
		slowEntered = make(chan struct{})
		release     = make(chan struct{})
		slowOnce    sync.Once
	)
	agentHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(req.URL.Path, "/logs") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-req.Context().Done()
			return
		}
		slowOnce.Do(func() { close(slowEntered) })
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `{"metadata":{"continue":""},"items":[]}`)
	}))
	defer agentHTTP.Close()
	r := startApp(t, nil, nil)
	ag, err := model.NewAgentFrom(model.AgentParams{ID: "agent-1", Name: "agent-1", Endpoint: agentHTTP.URL, EndpointType: 2, APIVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.app.store.UpsertAgent(context.Background(), ag); err != nil {
		t.Fatal(err)
	}
	token := bearerFor(t, r.app, r.cfg, "viewer", enum.AgentsGet)
	ui := r.app.ui.Addr
	waitFor(t, 10*time.Second, "the UI listener binds", func() bool { return ui() != "" })
	header := http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/json"}}

	// ends reports when the response of path ends, after its headers arrived.
	open := func(path string) (ended <-chan error) {
		req, err := http.NewRequest(http.MethodGet, "http://"+ui()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = header.Clone()
		req.Header.Set("Accept", "text/event-stream")
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d, want 200", path, resp.StatusCode)
		}
		done := make(chan error, 1)
		go func() {
			_, err := io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			done <- err
		}()
		return done
	}
	events := open("/api/v1/events/stream")
	logs := open("/api/v1/agents/agent-1/tasks/task/logs/stream?task_uid=u1")

	type answer struct {
		status int
		err    error
	}
	slow := make(chan answer, 1)
	go func() {
		status, _, err := get(ui(), "/api/v1/agents/agent-1/tasks?limit=2", header)
		slow <- answer{status, err}
	}()
	select {
	case <-slowEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow request did not reach the agent within 10s")
	}

	r.cancel()
	for name, ended := range map[string]<-chan error{"event stream": events, "log stream": logs} {
		select {
		case err := <-ended:
			if err != nil {
				t.Fatalf("the %s ended with %v, want a clean end", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s did not end within 10s of the shutdown", name)
		}
	}
	select {
	case <-r.done:
		t.Fatalf("Run returned (%v) while a request was in flight", r.err)
	case a := <-slow:
		t.Fatalf("the slow request ended (%d, %v) before it was released", a.status, a.err)
	default:
	}
	close(release)
	select {
	case a := <-slow:
		if a.err != nil || a.status != http.StatusOK {
			t.Fatalf("the request in flight: %d, %v; want 200", a.status, a.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request in flight did not finish within 10s of its release")
	}
	<-r.done
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Run: %v, want context.Canceled", r.err)
	}
	if _, _, err := get(ui(), "/login", nil); err == nil {
		t.Fatal("the UI listener answered after Run returned")
	}
}
