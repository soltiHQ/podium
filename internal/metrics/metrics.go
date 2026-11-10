package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/soltiHQ/control-plane/internal/buildinfo"
)

// The values of the transport label of podium_discovery_syncs_total.
const (
	TransportHTTP = "http"
	TransportGRPC = "grpc"
)

// The values of the outcome label of podium_discovery_syncs_total: how a sync
// ended. A request that is not a sync, such as a GET of the sync path, has none.
const (
	// SyncOK is a sync of an agent that podium knew.
	SyncOK = "ok"
	// SyncEnrolled is a sync that added a new agent.
	SyncEnrolled = "enrolled"
	// SyncUnauthenticated is a missing or wrong token.
	SyncUnauthenticated = "unauthenticated"
	// SyncRefused is a first contact whose token the enrollment secret does not derive.
	SyncRefused = "refused"
	// SyncInvalid is a payload that fails validation, the capability caps and the
	// token length included, or a body that cannot be read or parsed.
	SyncInvalid = "invalid"
	// SyncRateLimited is a sync that the sync bucket of its agent, or the limit on
	// token failures, refused.
	SyncRateLimited = "rate_limited"
	// SyncUnavailable is a sync that this node could not serve: it does not lead,
	// no leader is known, the leader is unreachable, or the store failed.
	SyncUnavailable = "unavailable"
)

// The values of the outcome label of podium_logins_total.
const (
	LoginOK      = "ok"
	LoginFailed  = "failed"
	LoginLimited = "limited"
)

// The values of the outcome label of podium_sync_turns_total: how an agent's turn
// of the sync runner ended.
const (
	// TurnAnswered is a turn in which the agent answered a call.
	TurnAnswered = "answered"
	// TurnUnanswered is a turn in which a call got no answer.
	TurnUnanswered = "unanswered"
	// TurnLocal is a turn that needed no call to the agent.
	TurnLocal = "local"
)

// commitBuckets are the buckets of podium_store_commit_duration_seconds, from
// 0.5 ms to 1 s: a commit syncs a disk, or a quorum.
var commitBuckets = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1}

// turnBuckets are the buckets of podium_sync_turn_duration_seconds: a turn makes
// calls to an agent, each within the push timeout.
var turnBuckets = []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}

// Metrics is the registry of podium and the instruments that record into it.
type Metrics struct {
	reg *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	grpcRequests *prometheus.CounterVec
	grpcDuration *prometheus.HistogramVec
	syncs        *prometheus.CounterVec
	logins       *prometheus.CounterVec
	turns        *prometheus.CounterVec
	turnDuration prometheus.Histogram
	storeCommit  *prometheus.HistogramVec
}

// New returns the metrics of a podium whose build is info, with the Go and process
// collectors registered.
func New(info buildinfo.Info) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "podium_http_requests_total",
			Help: "HTTP requests by listener (ui or discovery), matched mux route (other when none), method and status code.",
		}, []string{"server", "route", "method", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "podium_http_request_duration_seconds",
			Help:    "HTTP request duration by listener, matched mux route and method; streams are not timed.",
			Buckets: prometheus.DefBuckets,
		}, []string{"server", "route", "method"}),
		grpcRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "podium_grpc_requests_total",
			Help: "gRPC calls by full method and status code.",
		}, []string{"method", "code"}),
		grpcDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "podium_grpc_request_duration_seconds",
			Help:    "Unary gRPC call duration by full method; streams are not timed.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method"}),
		syncs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "podium_discovery_syncs_total",
			Help: "Agent discovery syncs this node handled, by transport and outcome.",
		}, []string{"transport", "outcome"}),
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "podium_logins_total",
			Help: "UI login attempts by outcome.",
		}, []string{"outcome"}),
		turns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "podium_sync_turns_total",
			Help: "Agent turns of the rollout sync runner, on the leader, by outcome.",
		}, []string{"outcome"}),
		turnDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "podium_sync_turn_duration_seconds",
			Help:    "Duration of an agent turn of the rollout sync runner, on the leader.",
			Buckets: turnBuckets,
		}),
		storeCommit: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "podium_store_commit_duration_seconds",
			Help:    "Duration of a store commit by backend: the durable commit, or the raft apply on the leader.",
			Buckets: commitBuckets,
		}, []string{"backend"}),
	}
	commit := info.Commit
	if commit == "" {
		commit = "unknown"
	}
	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "podium_build_info",
		Help:        "The build of this podium: its version, commit and Go toolchain; the value is 1.",
		ConstLabels: prometheus.Labels{"version": info.Version, "commit": commit, "goversion": info.GoVersion},
	})
	build.Set(1)
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		build,
		m.httpRequests, m.httpDuration, m.grpcRequests, m.grpcDuration,
		m.syncs, m.logins, m.turns, m.turnDuration, m.storeCommit,
	)
	// The series that are known in advance exist from the start, at zero, so that
	// a rate over them has a first sample and an alert can name them.
	for _, transport := range []string{TransportHTTP, TransportGRPC} {
		for _, outcome := range []string{SyncOK, SyncEnrolled, SyncUnauthenticated, SyncRefused, SyncInvalid, SyncRateLimited, SyncUnavailable} {
			m.syncs.WithLabelValues(transport, outcome)
		}
	}
	for _, outcome := range []string{LoginOK, LoginFailed, LoginLimited} {
		m.logins.WithLabelValues(outcome)
	}
	for _, outcome := range []string{TurnAnswered, TurnUnanswered, TurnLocal} {
		m.turns.WithLabelValues(outcome)
	}
	return m
}

// Handler serves the exposition of the registry. A collector that fails leaves
// its own metrics out and not the others.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

// Register adds c to the registry, which a raft node uses for the metrics it
// reads at scrape. It panics when c cannot be registered, as that is a bug of
// the build.
func (m *Metrics) Register(c prometheus.Collector) {
	if m == nil {
		return
	}
	m.reg.MustRegister(c)
}

// Sync counts a discovery sync of transport that ended with outcome.
func (m *Metrics) Sync(transport, outcome string) {
	if m == nil {
		return
	}
	m.syncs.WithLabelValues(transport, outcome).Inc()
}

// Login counts a login attempt that ended with outcome.
func (m *Metrics) Login(outcome string) {
	if m == nil {
		return
	}
	m.logins.WithLabelValues(outcome).Inc()
}

// SyncTurn counts an agent turn of the sync runner that ended with outcome, and
// records its duration d.
func (m *Metrics) SyncTurn(outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.turns.WithLabelValues(outcome).Inc()
	m.turnDuration.Observe(d.Seconds())
}

// StoreCommit returns what records the duration of a commit of backend, "durable"
// or "raft".
func (m *Metrics) StoreCommit(backend string) func(time.Duration) {
	if m == nil {
		return func(time.Duration) {}
	}
	h := m.storeCommit.WithLabelValues(backend)
	return func(d time.Duration) { h.Observe(d.Seconds()) }
}

// StoreFailed registers podium_store_failed, which is 1 at a scrape where failed
// returns true and 0 otherwise.
func (m *Metrics) StoreFailed(failed func() bool) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "podium_store_failed",
		Help: "1 while the store has failed and serves nothing, as /readyz reports it, otherwise 0.",
	}, func() float64 {
		if failed() {
			return 1
		}
		return 0
	}))
}
