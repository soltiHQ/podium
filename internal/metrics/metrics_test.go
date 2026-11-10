package metrics

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/buildinfo"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// sample is one line of the exposition: a series, and for a histogram one of its
// _bucket, _sum and _count lines.
type sample struct {
	name   string
	labels map[string]string
	value  float64
}

// exposition is what GET /metrics answered: the TYPE of each family, and the
// samples.
type exposition struct {
	types   map[string]string
	samples []sample
	text    string
}

// scrape returns the parsed exposition of m.
func scrape(t *testing.T, m *Metrics) exposition {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics: %d %s", w.Code, w.Body)
	}
	e := exposition{types: map[string]string{}, text: w.Body.String()}
	for _, line := range strings.Split(e.text, "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			e.types[f[2]] = f[3]
		case strings.HasPrefix(line, "#"):
		default:
			e.samples = append(e.samples, parseSample(t, line))
		}
	}
	return e
}

// parseSample parses name{key="value",...} number, where a value may hold any
// character but an unescaped quote.
func parseSample(t *testing.T, line string) sample {
	t.Helper()
	s := sample{labels: map[string]string{}}
	rest := line
	if i := strings.IndexAny(line, "{ "); line[i] == '{' {
		s.name, rest = line[:i], line[i+1:]
		for !strings.HasPrefix(rest, "}") {
			eq := strings.Index(rest, `="`)
			key := rest[:eq]
			rest = rest[eq+2:]
			var val strings.Builder
			for j := 0; ; j++ {
				if rest[j] == '\\' {
					j++
					val.WriteByte(rest[j])
					continue
				}
				if rest[j] == '"' {
					rest = strings.TrimPrefix(rest[j+1:], ",")
					break
				}
				val.WriteByte(rest[j])
			}
			s.labels[key] = val.String()
		}
		rest = rest[1:]
	} else {
		s.name, rest = line[:i], line[i:]
	}
	if _, err := fmt.Sscan(rest, &s.value); err != nil {
		t.Fatalf("sample %q: %v", line, err)
	}
	return s
}

// family returns the samples of the family name: for a histogram its _bucket, _sum
// and _count lines too.
func (e exposition) family(name string) []sample {
	var out []sample
	for _, s := range e.samples {
		if s.name == name || (e.types[name] == "histogram" && (s.name == name+"_bucket" || s.name == name+"_sum" || s.name == name+"_count")) {
			out = append(out, s)
		}
	}
	return out
}

// labelNames returns the sorted, comma-joined label names of the series of the
// family name, the le of a bucket aside, and fails the test when its series do
// not all carry the same ones.
func (e exposition) labelNames(t *testing.T, name string) string {
	t.Helper()
	var want string
	for i, s := range e.family(name) {
		var names []string
		for k := range s.labels {
			if k != "le" {
				names = append(names, k)
			}
		}
		sort.Strings(names)
		got := strings.Join(names, ",")
		if i > 0 && got != want {
			t.Fatalf("%s: series with label names %q and %q", name, want, got)
		}
		want = got
	}
	return want
}

// value returns the value of the series of the family name whose labels are
// exactly labels, for a histogram its count, and whether there is one.
func (e exposition) value(name string, labels map[string]string) (float64, bool) {
	if e.types[name] == "histogram" {
		name += "_count"
	}
	for _, s := range e.samples {
		if s.name != name || len(s.labels) != len(labels) {
			continue
		}
		match := true
		for k, v := range labels {
			if s.labels[k] != v {
				match = false
			}
		}
		if match {
			return s.value, true
		}
	}
	return 0, false
}

// serve answers a request of method to path through the ui chain of m around mux.
func serve(m *Metrics, server string, mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	m.HTTP(server)(Route(mux)).ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func testMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.PathValue("id"))) })
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusSeeOther) })
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: x\n\n"))
	})
	return mux
}

// Every name starts with podium_ besides the Go and process collectors, and each
// family has the type and the label names the metrics table of the server README
// documents.
func TestMetricsExposition(t *testing.T) {
	m := New(buildinfo.Info{Version: "v1.2.3", Commit: "0123456789ab", GoVersion: "go1.27.3"})
	mux := testMux()
	serve(m, "ui", mux, http.MethodGet, "/api/v1/agents/a-1")
	serve(m, "discovery", mux, http.MethodPost, "/login")

	info := &grpc.UnaryServerInfo{FullMethod: "/solti.discover.v1.DiscoverService/Sync"}
	_, _ = m.UnaryInterceptor()(context.Background(), nil, info, func(context.Context, any) (any, error) { return nil, nil })

	m.Sync(TransportHTTP, SyncOK)
	m.Login(LoginOK)
	m.SyncTurn(TurnAnswered, 20*time.Millisecond)
	m.StoreCommit("durable")(2 * time.Millisecond)
	m.StoreFailed(func() bool { return false })

	store := inmemory.New()
	a, err := model.NewAgent("agent-1", "agent-1", "http://10.0.0.1:8085")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.UpsertAgent(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	m.Inventory(store)

	e := scrape(t, m)
	want := map[string]struct{ typ, labels string }{
		"podium_build_info":                    {"gauge", "commit,goversion,version"},
		"podium_http_requests_total":           {"counter", "code,method,route,server"},
		"podium_http_request_duration_seconds": {"histogram", "method,route,server"},
		"podium_grpc_requests_total":           {"counter", "code,method"},
		"podium_grpc_request_duration_seconds": {"histogram", "method"},
		"podium_discovery_syncs_total":         {"counter", "outcome,transport"},
		"podium_logins_total":                  {"counter", "outcome"},
		"podium_agents":                        {"gauge", "status"},
		"podium_rollouts":                      {"gauge", "status"},
		"podium_sync_turns_total":              {"counter", "outcome"},
		"podium_sync_turn_duration_seconds":    {"histogram", ""},
		"podium_store_commit_duration_seconds": {"histogram", "backend"},
		"podium_store_failed":                  {"gauge", ""},
	}
	for name, w := range want {
		if e.types[name] != w.typ {
			t.Errorf("%s is a %q, want a %q", name, e.types[name], w.typ)
			continue
		}
		if got := e.labelNames(t, name); got != w.labels {
			t.Errorf("%s has labels %q, want %q", name, got, w.labels)
		}
	}
	for name := range e.types {
		if !strings.HasPrefix(name, "podium_") && !strings.HasPrefix(name, "go_") && !strings.HasPrefix(name, "process_") {
			t.Errorf("%s: a name outside podium_, go_ and process_", name)
		}
		if _, ok := want[name]; !ok && strings.HasPrefix(name, "podium_") {
			t.Errorf("%s is exposed and not in the table of this test", name)
		}
	}
	if v, _ := e.value("podium_build_info", map[string]string{"version": "v1.2.3", "commit": "0123456789ab", "goversion": "go1.27.3"}); v != 1 {
		t.Errorf("podium_build_info = %v, want 1", v)
	}
	if v, ok := e.value("podium_discovery_syncs_total", map[string]string{"transport": "grpc", "outcome": "unavailable"}); !ok || v != 0 {
		t.Errorf("a sync outcome that never happened: %v, %v; want a series at 0", v, ok)
	}
	if e.types["go_goroutines"] == "" {
		t.Error("the Go collector is not registered")
	}
	if _, ok := e.value("podium_http_requests_total", map[string]string{"server": "ui", "route": "GET /api/v1/agents/{id}", "method": "GET", "code": "200"}); !ok {
		t.Errorf("the ui request has no series under its mux pattern:\n%s", e.text)
	}
}

// A path that matches no route has one route label, "other", however many
// unknown paths and methods are asked, and a path value never becomes a label.
func TestMetricsRouteLabelIsBounded(t *testing.T) {
	m := New(buildinfo.Info{})
	mux := testMux()
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 1000 {
		path := fmt.Sprintf("/%x/%x", rng.Uint64(), i)
		method := []string{http.MethodGet, http.MethodPost, "PROPFIND", fmt.Sprintf("X%d", i)}[i%4]
		serve(m, "ui", mux, method, path)
	}
	serve(m, "ui", mux, http.MethodGet, "/api/v1/agents/secret-agent-id")

	e := scrape(t, m)
	routes, methods := map[string]bool{}, map[string]bool{}
	for _, s := range e.family("podium_http_requests_total") {
		routes[s.labels["route"]] = true
		methods[s.labels["method"]] = true
	}
	if len(routes) != 2 || !routes["other"] || !routes["GET /api/v1/agents/{id}"] {
		t.Fatalf("routes %v, want the one pattern and other", routes)
	}
	for method := range methods {
		if strings.HasPrefix(method, "X") || method == "PROPFIND" {
			t.Fatalf("an unknown method %q is a label", method)
		}
	}
	if strings.Contains(e.text, "secret-agent-id") {
		t.Fatal("a path value reached the exposition")
	}
}

// A stream counts in the requests and has no duration: it lasts as long as its
// client stays.
func TestStreamsCountRequestsOnly(t *testing.T) {
	m := New(buildinfo.Info{})
	mux := testMux()
	serve(m, "ui", mux, http.MethodGet, "/events")
	serve(m, "ui", mux, http.MethodGet, "/api/v1/agents/a")

	e := scrape(t, m)
	if v, ok := e.value("podium_http_requests_total", map[string]string{"server": "ui", "route": "GET /events", "method": "GET", "code": "200"}); !ok || v != 1 {
		t.Fatalf("the stream is not counted: %v %v", v, ok)
	}
	if _, ok := e.value("podium_http_request_duration_seconds", map[string]string{"server": "ui", "route": "GET /events", "method": "GET"}); ok {
		t.Fatal("the stream has a duration series")
	}
	if _, ok := e.value("podium_http_request_duration_seconds", map[string]string{"server": "ui", "route": "GET /api/v1/agents/{id}", "method": "GET"}); !ok {
		t.Fatal("the plain request has no duration series")
	}
}

// The gRPC interceptors count each call by method and code and time the unary
// ones; a stream is counted and not timed.
func TestGRPCInterceptors(t *testing.T) {
	m := New(buildinfo.Info{})
	info := &grpc.UnaryServerInfo{FullMethod: "/solti.discover.v1.DiscoverService/Sync"}
	_, _ = m.UnaryInterceptor()(context.Background(), nil, info, func(context.Context, any) (any, error) { return nil, nil })
	_, _ = m.UnaryInterceptor()(context.Background(), nil, info, func(context.Context, any) (any, error) {
		return nil, status.Error(codes.ResourceExhausted, "slow down")
	})
	_ = m.StreamInterceptor()(nil, nil, &grpc.StreamServerInfo{FullMethod: "/grpc.health.v1.Health/Watch"}, func(any, grpc.ServerStream) error { return nil })

	e := scrape(t, m)
	for code, want := range map[string]float64{"OK": 1, "ResourceExhausted": 1} {
		if v, _ := e.value("podium_grpc_requests_total", map[string]string{"method": info.FullMethod, "code": code}); v != want {
			t.Errorf("sync calls with code %s: %v, want %v", code, v, want)
		}
	}
	if v, _ := e.value("podium_grpc_request_duration_seconds", map[string]string{"method": info.FullMethod}); v != 2 {
		t.Errorf("timed sync calls: %v, want 2", v)
	}
	if _, ok := e.value("podium_grpc_request_duration_seconds", map[string]string{"method": "/grpc.health.v1.Health/Watch"}); ok {
		t.Error("the stream has a duration series")
	}
	if v, _ := e.value("podium_grpc_requests_total", map[string]string{"method": "/grpc.health.v1.Health/Watch", "code": "OK"}); v != 1 {
		t.Errorf("the stream is not counted: %v", v)
	}
}

// The inventory is read when the scrape runs, one count per kind, and a store that
// fails to count takes its gauges out of the exposition without failing the scrape.
func TestInventoryIsReadAtScrape(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	m := New(buildinfo.Info{})
	m.Inventory(store)

	if v, ok := scrape(t, m).value("podium_agents", map[string]string{"status": "active"}); !ok || v != 0 {
		t.Fatalf("an empty store: active agents %v, %v; want a zero series", v, ok)
	}
	for i, st := range []enum.AgentStatus{enum.AgentStatusActive, enum.AgentStatusActive, enum.AgentStatusInactive, enum.AgentStatusDisconnected} {
		a, err := model.NewAgent(fmt.Sprintf("agent-%d", i), "n", "http://10.0.0.1:8085")
		if err != nil {
			t.Fatal(err)
		}
		a.MarkStatus(st)
		if err = store.UpsertAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	r, err := model.NewRollout("spec-1", "agent-0", 1)
	if err != nil {
		t.Fatal(err)
	}
	r.MarkFailed("boom")
	if err = store.UpsertRollout(ctx, r); err != nil {
		t.Fatal(err)
	}

	e := scrape(t, m)
	for status, want := range map[string]float64{"active": 2, "inactive": 1, "disconnected": 1} {
		if v, _ := e.value("podium_agents", map[string]string{"status": status}); v != want {
			t.Errorf("%s agents: %v, want %v", status, v, want)
		}
	}
	for status, want := range map[string]float64{"pending": 0, "synced": 0, "drift": 0, "failed": 1, "unknown": 0} {
		if v, ok := e.value("podium_rollouts", map[string]string{"status": status}); !ok || v != want {
			t.Errorf("%s rollouts: %v, %v; want %v", status, v, ok, want)
		}
	}

	broken := New(buildinfo.Info{})
	broken.Inventory(failingStore{inmemory.New()})
	e = scrape(t, broken)
	if e.types["podium_agents"] != "" || e.types["podium_rollouts"] != "" {
		t.Fatal("a store that cannot count still has inventory series")
	}
}

// failingStore counts nothing: its counts fail like a store that stopped serving.
type failingStore struct{ *inmemory.Store }

func (failingStore) CountAgents(context.Context) (map[enum.AgentStatus]int, error) {
	return nil, errors.New("store failed")
}

func (failingStore) CountRollouts(context.Context) (map[enum.SyncStatus]int, error) {
	return nil, errors.New("store failed")
}

// podium_store_failed follows the function it is given, at each scrape.
func TestStoreFailedGaugeFollowsTheFunction(t *testing.T) {
	m := New(buildinfo.Info{})
	failed := false
	m.StoreFailed(func() bool { return failed })
	for _, want := range []float64{0, 1, 0} {
		failed = want == 1
		if v, ok := scrape(t, m).value("podium_store_failed", map[string]string{}); !ok || v != want {
			t.Fatalf("podium_store_failed %v, %v; want %v", v, ok, want)
		}
	}
}

// The store commit histogram runs from 0.5 ms to 1 s.
func TestStoreCommitBuckets(t *testing.T) {
	m := New(buildinfo.Info{})
	m.StoreCommit("durable")(time.Millisecond)
	var bounds []string
	for _, s := range scrape(t, m).family("podium_store_commit_duration_seconds") {
		if s.name == "podium_store_commit_duration_seconds_bucket" {
			bounds = append(bounds, s.labels["le"])
		}
	}
	if len(bounds) < 2 || bounds[0] != "0.0005" || bounds[len(bounds)-2] != "1" || bounds[len(bounds)-1] != "+Inf" {
		t.Fatalf("buckets %v, want 0.0005 first and 1 last before +Inf", bounds)
	}
}

// A nil *Metrics does nothing and breaks nothing: the builders that tests call
// without metrics pass nil.
func TestNilMetricsIsInert(t *testing.T) {
	var m *Metrics
	m.Sync(TransportHTTP, SyncOK)
	m.Login(LoginOK)
	m.SyncTurn(TurnAnswered, time.Second)
	m.StoreCommit("durable")(time.Second)
	m.StoreFailed(func() bool { return true })
	m.Inventory(inmemory.New())
	w := httptest.NewRecorder()
	h := m.HTTP("ui")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusTeapot {
		t.Fatalf("a nil Metrics changed the answer: %d", w.Code)
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/x/Y"}
	if _, err := m.UnaryInterceptor()(context.Background(), nil, info, func(context.Context, any) (any, error) { return 7, nil }); err != nil {
		t.Fatal(err)
	}
}
