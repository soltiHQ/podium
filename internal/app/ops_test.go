package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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

// A node is not ready while its durable store has failed. The failure is injected
// at the seam of the readiness; the store's own failure is the durable package's
// test.
func TestReadinessFollowsStore(t *testing.T) {
	var broken atomic.Bool
	r := startApp(t, nil, func(a *App) {
		a.ready.failure = func() error {
			if broken.Load() {
				return errors.New("injected durable failure")
			}
			return nil
		}
	})
	ops := r.app.ops.Addr
	waitFor(t, 10*time.Second, "the ops listener binds", func() bool { return ops() != "" })
	opsAnswers(t, ops(), "/readyz", 200, "ok")

	broken.Store(true)
	opsAnswers(t, ops(), "/readyz", 503, "storage failed")
	opsAnswers(t, ops(), "/livez", 200, "ok") // a failed store is not a dead process
	broken.Store(false)
	opsAnswers(t, ops(), "/readyz", 200, "ok")
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
// authentication: a failed store.
func TestGRPCHealthFollowsReadiness(t *testing.T) {
	var broken atomic.Bool
	r := startApp(t, nil, func(a *App) {
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
}
