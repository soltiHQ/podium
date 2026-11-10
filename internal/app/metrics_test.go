package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/buildinfo"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/metrics"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// scrapeOps returns the exposition that GET /metrics of the ops listener at addr
// answers, with no credentials.
func scrapeOps(t *testing.T, addr string) string {
	t.Helper()
	status, body, err := get(addr, "/metrics", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET /metrics: %d, %v\n%s", status, err, body)
	}
	return body
}

// series returns the value of the sample whose name and labels are exactly series,
// such as `podium_agents{status="active"}`: the labels sorted by name, as the
// exposition writes them.
func series(body, series string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, series+" "); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			return v, err == nil
		}
	}
	return 0, false
}

// seriesOf returns the lines of body that start with prefix.
func seriesOf(body, prefix string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

// derivedToken is the token the enrollment secret derives for id.
func derivedToken(secret, id string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil))
}

// agentSyncs sends syncs to a running app over its real discovery listeners.
type agentSyncs struct {
	t      *testing.T
	httpAt string
	grpc   discoverv1.DiscoverServiceClient
}

func newAgentSyncs(t *testing.T, r *runningApp) agentSyncs {
	t.Helper()
	waitFor(t, 10*time.Second, "the discovery listeners bind", func() bool {
		return r.app.listeners[1].Addr() != "" && r.app.grpc.Addr() != ""
	})
	conn, err := grpc.NewClient("passthrough:///"+r.app.grpc.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentSyncs{t: t, httpAt: r.app.listeners[1].Addr(), grpc: discoverv1.NewDiscoverServiceClient(conn)}
}

// overHTTP syncs agent id with token and returns the status of the answer.
func (a agentSyncs) overHTTP(id, token string) int {
	a.t.Helper()
	body := `{"id":"` + id + `","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
	req, err := http.NewRequest(http.MethodPost, "http://"+a.httpAt+"/api/v1/discovery/sync", strings.NewReader(body))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// overGRPC syncs agent id with token and returns the code of the answer.
func (a agentSyncs) overGRPC(id, token string) codes.Code {
	a.t.Helper()
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	_, err := a.grpc.Sync(ctx, &discoverv1.SyncRequest{
		Id: id, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1,
	})
	return status.Code(err)
}

// The ops listener serves the exposition without credentials, which has the
// build, the Go runtime, the inventory and the requests of the listeners that
// answered; the UI listener does not serve it.
func TestMetricsExposition(t *testing.T) {
	r := startApp(t, nil, nil)
	ops, ui := r.app.ops.Addr, r.app.ui.Addr
	waitFor(t, 10*time.Second, "the listeners bind", func() bool { return ops() != "" && ui() != "" })
	opsAnswers(t, ops(), "/readyz", 200, "ok")
	if _, _, err := get(ui(), "/login", nil); err != nil {
		t.Fatal(err)
	}
	syncs := newAgentSyncs(t, r)
	if code := syncs.overHTTP("agent-1", "any-token"); code != http.StatusOK {
		t.Fatalf("first sync: HTTP %d, want 200", code)
	}
	if code := syncs.overGRPC("agent-1", "any-token"); code != codes.OK {
		t.Fatalf("second sync: %v, want OK", code)
	}

	body := scrapeOps(t, ops())
	for _, want := range []string{
		`podium_build_info{commit=`,
		`go_goroutines `,
		`podium_agents{status="active"} 1`,
		`podium_agents{status="inactive"} 0`,
		`podium_rollouts{status="pending"} 0`,
		`podium_store_failed 0`,
		`podium_http_requests_total{code="200",method="GET",route="/login",server="ui"} 1`,
		`podium_http_requests_total{code="200",method="POST",route="/api/v1/discovery/sync",server="discovery"} 1`,
		`podium_grpc_requests_total{code="OK",method="/solti.discover.v1.DiscoverService/Sync"} 1`,
		`podium_discovery_syncs_total{outcome="enrolled",transport="http"} 1`,
		`podium_discovery_syncs_total{outcome="ok",transport="grpc"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition lacks %q", want)
		}
	}
	for _, absent := range []string{"podium_raft_"} {
		if strings.Contains(body, absent) {
			t.Errorf("a standalone node exposes %s", absent)
		}
	}
	// The build info names this binary, and the exposition holds no secret.
	info := buildinfo.Read()
	commit := info.Commit
	if commit == "" {
		commit = "unknown"
	}
	if v, ok := series(body, fmt.Sprintf(`podium_build_info{commit=%q,goversion=%q,version=%q}`, commit, info.GoVersion, info.Version)); !ok || v != 1 {
		t.Errorf("podium_build_info of this build: %v, %v; want 1", v, ok)
	}
	for _, secret := range []string{"any-token", r.cfg.Auth.JWTSecret} {
		if strings.Contains(body, secret) {
			t.Errorf("the exposition shows %q", secret)
		}
	}
	// The UI listener does not serve the ops routes.
	if status, body, err := get(ui(), "/metrics", nil); err != nil || strings.Contains(body, "podium_build_info") {
		t.Errorf("GET /metrics on the UI listener: %d, %v, an exposition", status, err)
	}
}

// No label takes the value of a path or of an agent: after 1000 unknown paths on
// each listener there is one series for them, and after 50 agents synced over both
// transports no agent ID appears anywhere.
func TestMetricsLabelsAreBounded(t *testing.T) {
	r := startApp(t, nil, nil)
	waitFor(t, 10*time.Second, "the listeners bind", func() bool { return r.app.ops.Addr() != "" && r.app.ui.Addr() != "" })
	syncs := newAgentSyncs(t, r)

	rng := rand.New(rand.NewPCG(7, 8))
	for i := range 1000 {
		path := fmt.Sprintf("/%x/%d", rng.Uint64(), i)
		for _, addr := range []string{r.app.ui.Addr(), syncs.httpAt} {
			if _, _, err := get(addr, path, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range 50 {
		id := fmt.Sprintf("agent-secret-%02d", i)
		if i%2 == 0 {
			syncs.overHTTP(id, "token-"+id)
		} else {
			syncs.overGRPC(id, "token-"+id)
		}
	}

	body := scrapeOps(t, r.app.ops.Addr())
	if strings.Contains(body, "agent-secret-") || strings.Contains(body, "token-agent") {
		t.Fatal("an agent ID or a token reached the exposition")
	}
	for _, server := range []string{"ui", "discovery"} {
		var routes []string
		for _, line := range seriesOf(body, "podium_http_requests_total{") {
			if strings.Contains(line, `server="`+server+`"`) && strings.Contains(line, `method="GET"`) {
				routes = append(routes, line)
			}
		}
		if len(routes) != 1 {
			t.Errorf("%s: %d series for 1000 unknown GET paths, want 1: %v", server, len(routes), routes)
		}
	}
	// The discovery listener has no catch-all route: its unknown paths are "other".
	if v, ok := series(body, `podium_http_requests_total{code="404",method="GET",route="other",server="discovery"}`); !ok || v != 1000 {
		t.Errorf("unknown discovery paths under route other: %v, %v; want 1000", v, ok)
	}
	if v, _ := series(body, `podium_agents{status="active"}`); v != 50 {
		t.Errorf("active agents %v, want 50", v)
	}
}

// Each outcome of a sync is counted on its transport: with an enrollment secret,
// an unknown ID with a made-up token is refused, with its derived token it is
// enrolled, then ok; a wrong token of an enrolled agent is unauthenticated, an
// invalid ID is invalid, and an agent that syncs over its budget, on either
// transport, is rate limited.
func TestDiscoveryOutcomesCounted(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	r := startApp(t, func(c *config.Config) { c.AgentAuth.EnrollSecret = secret }, nil)
	syncs := newAgentSyncs(t, r)
	ops := r.app.ops.Addr

	httpCode := func(want int, id, token string) {
		t.Helper()
		if got := syncs.overHTTP(id, token); got != want {
			t.Fatalf("HTTP sync of %q: %d, want %d", id, got, want)
		}
	}
	grpcCode := func(want codes.Code, id, token string) {
		t.Helper()
		if got := syncs.overGRPC(id, token); got != want {
			t.Fatalf("gRPC sync of %q: %v, want %v", id, got, want)
		}
	}
	httpCode(http.StatusUnauthorized, "agent-1", "made-up-token")
	httpCode(http.StatusOK, "agent-1", derivedToken(secret, "agent-1"))
	httpCode(http.StatusOK, "agent-1", derivedToken(secret, "agent-1"))
	httpCode(http.StatusUnauthorized, "agent-1", "wrong-token")
	httpCode(http.StatusBadRequest, "bad id", "token")

	grpcCode(codes.Unauthenticated, "agent-2", "made-up-token")
	grpcCode(codes.OK, "agent-2", derivedToken(secret, "agent-2"))
	grpcCode(codes.OK, "agent-2", derivedToken(secret, "agent-2"))
	grpcCode(codes.Unauthenticated, "agent-2", "wrong-token")
	grpcCode(codes.InvalidArgument, "bad id", "token")

	// One budget for both transports: the burst is spent over HTTP, and the next
	// gRPC sync of the agent finds it empty.
	token3 := derivedToken(secret, "agent-3")
	for range 12 {
		syncs.overHTTP("agent-3", token3)
	}
	grpcCode(codes.ResourceExhausted, "agent-3", token3)

	body := scrapeOps(t, ops())
	for series_, want := range map[string]float64{
		`podium_discovery_syncs_total{outcome="refused",transport="http"}`:         1,
		`podium_discovery_syncs_total{outcome="unauthenticated",transport="http"}`: 1,
		`podium_discovery_syncs_total{outcome="invalid",transport="http"}`:         1,
		`podium_discovery_syncs_total{outcome="refused",transport="grpc"}`:         1,
		`podium_discovery_syncs_total{outcome="enrolled",transport="grpc"}`:        1,
		`podium_discovery_syncs_total{outcome="unauthenticated",transport="grpc"}`: 1,
		`podium_discovery_syncs_total{outcome="invalid",transport="grpc"}`:         1,
		`podium_discovery_syncs_total{outcome="unavailable",transport="http"}`:     0,
		`podium_discovery_syncs_total{outcome="unavailable",transport="grpc"}`:     0,
	} {
		if v, ok := series(body, series_); !ok || v != want {
			t.Errorf("%s = %v, %v; want %v", series_, v, ok, want)
		}
	}
	// agent-1 enrolled once and synced ok once over HTTP; agent-3 enrolled
	// on its first sync, which is ok or enrolled depending on none other.
	if v, _ := series(body, `podium_discovery_syncs_total{outcome="enrolled",transport="http"}`); v != 2 {
		t.Errorf("HTTP enrolled %v, want 2 (agent-1 and agent-3)", v)
	}
	if v, _ := series(body, `podium_discovery_syncs_total{outcome="ok",transport="grpc"}`); v != 1 {
		t.Errorf("gRPC ok %v, want 1", v)
	}
	if v, _ := series(body, `podium_discovery_syncs_total{outcome="rate_limited",transport="http"}`); v < 1 {
		t.Errorf("HTTP rate_limited %v, want at least 1 of 12 syncs over a burst of 10", v)
	}
	if v, _ := series(body, `podium_discovery_syncs_total{outcome="rate_limited",transport="grpc"}`); v != 1 {
		t.Errorf("gRPC rate_limited %v, want 1", v)
	}
}

// A follower that cannot send a sync to a leader refuses it itself, and counts it as
// unavailable on either transport; the syncs a follower forwards are counted by the
// leader that serves them (the interceptor and middleware tests check that a
// forward is no refusal).
func TestFollowerCountsRefusedSyncsAsUnavailable(t *testing.T) {
	m := metrics.New(buildinfo.Read())
	cfg := config.Default()
	cfg.HTTPDiscovery.Addr = "127.0.0.1:0" // no port to forward to: the follower answers 503 itself
	hub := event.NewHub(zerolog.Nop())
	t.Cleanup(hub.Close)
	agents := agent.New(inmemory.New(), zerolog.Nop())
	h, srv := buildDiscoveryListeners(zerolog.Nop(), agents, hub, follower{leader: "127.0.0.1:7000"}, cfg, nil, nil, nil, nil, m)
	client := serveDiscovery(t, srv)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(`{"id":"agent-1"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP sync on a follower: %d, want 503", w.Code)
	}
	_, err := client.Sync(context.Background(), &discoverv1.SyncRequest{Id: "agent-1"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("gRPC sync on a follower: %v, want Unavailable", err)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, transport := range []string{"http", "grpc"} {
		if v, ok := series(rec.Body.String(), `podium_discovery_syncs_total{outcome="unavailable",transport="`+transport+`"}`); !ok || v != 1 {
			t.Errorf("%s unavailable = %v, %v; want 1", transport, v, ok)
		}
	}
}

// With three nodes, the leader exposes its raft state, term and indexes, the
// followers theirs and the time since the leader last spoke, and all of them the
// two other voters; a write commits through raft on the leader, which times it, and
// a follower's inventory is its local replica. No label names a node.
func TestRaftMetricsOnLeaderAndFollower(t *testing.T) {
	nodes := clusterNodes(t, "n1", "n2", "n3")
	for _, n := range nodes {
		n.start(t)
	}
	for _, n := range nodes {
		waitFor(t, 10*time.Second, n.id+" binds its ops listener", func() bool { return n.app.ops.Addr() != "" })
		opsAnswers(t, n.app.ops.Addr(), "/readyz", 200, "ok")
	}
	var leader *clusterNode
	waitFor(t, 10*time.Second, "one leader", func() bool {
		leader = nil
		for _, n := range nodes {
			if n.app.leadership.AmLeader() {
				if leader != nil {
					return false
				}
				leader = n
			}
		}
		return leader != nil
	})
	ag, err := model.NewAgent("agent-1", "agent-1", "http://10.0.0.5:8085")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "a write through the leader", func() bool {
		err := leader.app.store.UpsertAgent(context.Background(), ag)
		return err == nil
	})

	for _, n := range nodes {
		wantState, otherState := "follower", "leader"
		if n == leader {
			wantState, otherState = "leader", "follower"
		}
		waitFor(t, 10*time.Second, n.id+" shows the write and a contact with the leader", func() bool {
			body := scrapeOps(t, n.app.ops.Addr())
			agents, _ := series(body, `podium_agents{status="active"}`)
			_, contact := series(body, `podium_raft_last_contact_seconds`)
			return agents == 1 && (n == leader) != contact
		})
		body := scrapeOps(t, n.app.ops.Addr())
		for _, want := range []string{
			`podium_raft_state{state="` + wantState + `"} 1`,
			`podium_raft_state{state="` + otherState + `"} 0`,
			`podium_raft_state{state="candidate"} 0`,
			`podium_raft_peers 2`,
		} {
			if !strings.Contains(body, want+"\n") {
				t.Errorf("%s: the exposition lacks %q", n.id, want)
			}
		}
		for _, name := range []string{"podium_raft_term", "podium_raft_last_index", "podium_raft_applied_index"} {
			if v, ok := series(body, name); !ok || v < 1 {
				t.Errorf("%s: %s = %v, %v; want at least 1", n.id, name, v, ok)
			}
		}
		if v, ok := series(body, `podium_raft_last_contact_seconds`); n != leader && (!ok || v < 0 || v > 5) {
			t.Errorf("%s: last contact %v, %v; want a few seconds at most", n.id, v, ok)
		}
		for _, line := range seriesOf(body, "podium_raft_") {
			if strings.Contains(line, "n1") || strings.Contains(line, "n2") || strings.Contains(line, "n3") || strings.Contains(line, "127.0.0.1") {
				t.Errorf("%s: a raft series names a node: %s", n.id, line)
			}
		}
		commits, _ := series(body, `podium_store_commit_duration_seconds_count{backend="raft"}`)
		if n == leader && commits < 1 {
			t.Errorf("the leader timed %v raft commits, want at least 1", commits)
		}
		if n != leader && commits != 0 {
			t.Errorf("%s: a follower timed %v raft commits, want 0", n.id, commits)
		}
	}
}

// The failed-store gauge follows the hook that /readyz reads, at each scrape, and a
// durable store's commits are timed, those of the seed at start included.
func TestStoreFailedGauge(t *testing.T) {
	var broken atomic.Bool
	r := startApp(t, func(c *config.Config) {
		c.Cluster.Standalone.Ephemeral = false
		c.Cluster.Standalone.DataDir = filepath.Join(t.TempDir(), "state")
	}, func(a *App) {
		a.ready.failure = func() error {
			if broken.Load() {
				return errors.New("injected durable failure")
			}
			return nil
		}
	})
	waitFor(t, 10*time.Second, "the ops listener binds", func() bool { return r.app.ops.Addr() != "" })
	for _, want := range []float64{0, 1, 0} {
		broken.Store(want == 1)
		if v, ok := series(scrapeOps(t, r.app.ops.Addr()), "podium_store_failed"); !ok || v != want {
			t.Fatalf("podium_store_failed = %v, %v; want %v", v, ok, want)
		}
	}
	if v, _ := series(scrapeOps(t, r.app.ops.Addr()), `podium_store_commit_duration_seconds_count{backend="durable"}`); v < 1 {
		t.Fatalf("durable commits timed: %v, want the commits of the seed at least", v)
	}
}
