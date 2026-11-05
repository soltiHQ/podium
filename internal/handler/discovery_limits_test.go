package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	agentv1 "github.com/soltiHQ/control-plane/api/gen/solti/agent/v1"
	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// badRequest is a sync answered 400 over HTTP and InvalidArgument over gRPC.
const badRequest syncOutcome = "bad request"

// syncLimited is what a handler tells an agent that syncs faster than its bucket
// refills, and logs about it.
const syncLimited = "agent syncs too often: at most 10 at once, then one per 1s"

// syncResult is how a transport answered one sync.
type syncResult struct {
	outcome    syncOutcome
	message    string // the message of an answer that is not a success
	retryAfter string // the seconds of the Retry-After header over HTTP, or of the RetryInfo of the status over gRPC
}

// syncFunc sends req to a handler, with token as the bearer token when it is set.
type syncFunc func(req *discoverv1.SyncRequest, token string) syncResult

// manualClock is a clock that stands still until the test moves it.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// limitEnv is what the tests of the discovery limits build a handler from.
type limitEnv struct {
	svc         *agent.Service
	hub         *event.Hub
	logger      zerolog.Logger
	limiter     *ratelimit.Limiter
	syncs       *ratelimit.Buckets
	clock       *manualClock
	requireAuth bool
}

// newLimitEnv builds a discovery over store with the sync buckets podium uses
// and a clock that stands still, so that a refill is the test's to cause.
// Agent authentication is required, as by default. The failure limiter is roomy:
// its cap on token checks in flight plays no part in a burst of concurrent
// syncs, and the failures of a test never block a pair.
func newLimitEnv(t *testing.T, store storage.Storage) *limitEnv {
	t.Helper()
	hub := event.NewHub(zerolog.Nop())
	t.Cleanup(hub.Close)
	return &limitEnv{
		svc:         agent.New(store, zerolog.Nop()),
		hub:         hub,
		logger:      zerolog.Nop(),
		limiter:     ratelimit.New(ratelimit.Config{MaxAttempts: 1000, Window: time.Minute, BlockWindow: time.Minute}),
		syncs:       NewSyncBuckets(),
		clock:       &manualClock{now: time.Unix(1_800_000_000, 0)},
		requireAuth: true,
	}
}

// enroll gives each agent its token, as its first sync would, without a sync:
// the bucket of the test stays whole.
func (e *limitEnv) enroll(t *testing.T, tokens map[string]string) {
	t.Helper()
	for id, token := range tokens {
		if err := e.svc.VerifyOrEnrollToken(context.Background(), id, token); err != nil {
			t.Fatalf("enroll %s: %v", id, err)
		}
	}
}

func (e *limitEnv) httpSyncer() syncFunc {
	h := NewHTTPDiscovery(e.logger, e.svc, e.hub, e.limiter, e.syncs, nil, e.requireAuth)
	h.clock = e.clock.Now
	return func(req *discoverv1.SyncRequest, token string) syncResult {
		body, err := discoverySyncMarshal.Marshal(req)
		if err != nil {
			return syncResult{outcome: syncOutcome("marshal: " + err.Error())}
		}
		res := syncHTTP(h, string(body), token)
		var answer struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(res.Body.Bytes(), &answer) // the answer to a success has none
		out := syncResult{message: answer.Message, retryAfter: res.Header().Get("Retry-After")}
		switch res.Code {
		case http.StatusOK:
			out.outcome = synced
		case http.StatusTooManyRequests:
			out.outcome = refused
		case http.StatusUnauthorized:
			out.outcome = unauthenticated
		case http.StatusBadRequest:
			out.outcome = badRequest
		default:
			out.outcome = syncOutcome(fmt.Sprintf("HTTP %d", res.Code))
		}
		return out
	}
}

func (e *limitEnv) grpcSyncer() syncFunc {
	g := NewGRPCDiscovery(e.logger, e.svc, e.hub, e.limiter, e.syncs, nil, e.requireAuth)
	g.clock = e.clock.Now
	return func(req *discoverv1.SyncRequest, token string) syncResult {
		ctx := context.Background()
		if token != "" {
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
		}
		_, err := g.Sync(ctx, req)
		st := grpcstatus.Convert(err)
		out := syncResult{message: st.Message()}
		for _, detail := range st.Details() {
			if retry, ok := detail.(*errdetails.RetryInfo); ok {
				out.retryAfter = strconv.Itoa(int(retry.GetRetryDelay().AsDuration() / time.Second))
			}
		}
		switch grpcstatus.Code(err) {
		case codes.OK:
			out.outcome = synced
		case codes.ResourceExhausted:
			out.outcome = refused
		case codes.Unauthenticated:
			out.outcome = unauthenticated
		case codes.InvalidArgument:
			out.outcome = badRequest
		default:
			out.outcome = syncOutcome(err.Error())
		}
		return out
	}
}

// limitTransports are the two transports of discovery, which answer a sync that
// goes over a limit in their own ways.
var limitTransports = []struct {
	name       string
	syncer     func(*limitEnv) syncFunc
	retryAfter string                   // when a sync over the rate may come back, in seconds
	invalid    func(rule string) string // the message of the answer to data that breaks rule
}{
	{"http", (*limitEnv).httpSyncer, "1", func(rule string) string { return "Invalid agent data: " + rule + "." }},
	{"grpc", (*limitEnv).grpcSyncer, "1", func(rule string) string { return "invalid agent data: " + rule }},
}

// as makes a sync of heartbeat's agent come from the agent id.
func as(id string) func(*discoverv1.SyncRequest) {
	return func(r *discoverv1.SyncRequest) { r.Id = id }
}

// txCount counts the transactions opened through it, the unit the store commits
// in. A sync opens one to check its token, which writes nothing for an agent that
// is enrolled and so proposes nothing on the raft store, and one to write the
// agent row: that is the commit a refused sync must not cause.
type txCount struct {
	storage.Storage
	txs atomic.Int64
}

func (s *txCount) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	s.txs.Add(1)
	return s.Storage.WithTx(ctx, fn)
}

// An agent ID has a bucket of 10 syncs, refilled one per second. Thirty
// concurrent syncs of one agent land 10 and are answered 429 / ResourceExhausted
// 20 times, another agent has its own bucket, and after a second one more lands.
func TestSyncBurstIsLimitedPerAgent(t *testing.T) {
	const burst, attempts = 10, 30
	for _, tr := range limitTransports {
		t.Run(tr.name, func(t *testing.T) {
			env := newLimitEnv(t, inmemory.New())
			env.enroll(t, map[string]string{"agent-a": "token-a", "agent-b": "token-b"})
			send := tr.syncer(env)

			var (
				wg      sync.WaitGroup
				results = make(chan syncOutcome, attempts)
			)
			for range attempts {
				wg.Go(func() { results <- send(heartbeat(1, nil), "token-a").outcome })
			}
			wg.Wait()
			close(results)
			got := map[syncOutcome]int{}
			for outcome := range results {
				got[outcome]++
			}
			if got[synced] != burst || got[refused] != attempts-burst || len(got) != 2 {
				t.Fatalf("%d concurrent syncs of one agent ended %v, want %d synced and %d refused", attempts, got, burst, attempts-burst)
			}

			// What the agent is told: how long to wait, and why.
			if res := send(heartbeat(1, nil), "token-a"); res != (syncResult{outcome: refused, message: syncLimited, retryAfter: tr.retryAfter}) {
				t.Fatalf("a sync over the rate was answered %+v, want a refusal with %q and Retry-After %q", res, syncLimited, tr.retryAfter)
			}

			if res := send(heartbeat(1, as("agent-b")), "token-b"); res.outcome != synced {
				t.Fatalf("another agent while agent-a is limited: %s, want synced", res.outcome)
			}

			// A part of a second refills nothing; a second refills one token.
			env.clock.Advance(999 * time.Millisecond)
			if res := send(heartbeat(1, nil), "token-a"); res.outcome != refused {
				t.Fatalf("agent-a 999 ms after its burst: %s, want refused", res.outcome)
			}
			env.clock.Advance(time.Millisecond)
			if res := send(heartbeat(1, nil), "token-a"); res.outcome != synced {
				t.Fatalf("agent-a one second after its burst: %s, want synced", res.outcome)
			}
			if res := send(heartbeat(1, nil), "token-a"); res.outcome != refused {
				t.Fatalf("agent-a after the one token the second refilled: %s, want refused", res.outcome)
			}
		})
	}
}

// The sync that enrolls an agent ID is a sync like any other: it is charged one
// token, so it is the first of the burst of 10, not two of it.
func TestEnrollingSyncIsChargedOneToken(t *testing.T) {
	const burst = 10
	for _, tr := range limitTransports {
		t.Run(tr.name, func(t *testing.T) {
			env := newLimitEnv(t, inmemory.New())
			send := tr.syncer(env)

			for i := range burst {
				if res := send(heartbeat(1, nil), "token-a"); res.outcome != synced {
					t.Fatalf("sync %d of the burst: %s, want synced", i+1, res.outcome)
				}
				if token, err := env.svc.AgentToken(context.Background(), "agent-a"); err != nil || token != "token-a" {
					t.Fatalf("token of agent-a after sync %d: %q %v, want the one the first sync enrolled", i+1, token, err)
				}
			}
			if res := send(heartbeat(1, nil), "token-a"); res.outcome != refused {
				t.Fatalf("sync %d: %s, want refused: the enrollment cost more than one token", burst+1, res.outcome)
			}
		})
	}
}

// A sync over the rate is refused before the store is touched: before the token
// check, and so before the agent is written. Each transaction of the store is
// counted. An accepted sync opens one to check the token, when agent
// authentication is required, and one to write the agent; a refused sync opens
// none, with authentication required or not. The row of the agent stays what the
// last accepted sync made of it.
func TestLimitedSyncWritesNothing(t *testing.T) {
	const burst, refusals = 10, 5
	for _, tr := range limitTransports {
		for _, requireAuth := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, auth required %v", tr.name, requireAuth), func(t *testing.T) {
				ctx := context.Background()
				store := &txCount{Storage: inmemory.New()}
				env := newLimitEnv(t, store)
				env.requireAuth = requireAuth
				env.enroll(t, map[string]string{"agent-a": "token-a"})
				send := tr.syncer(env)

				var checks int64 // transactions that only check the token
				if requireAuth {
					checks = 1
				}
				opened := store.txs.Load()
				for i := range burst {
					if res := send(heartbeat(int64(100+i), nil), "token-a"); res.outcome != synced {
						t.Fatalf("sync %d of the burst: %s, want synced", i+1, res.outcome)
					}
				}
				if got, want := store.txs.Load()-opened, burst*(checks+1); got != want {
					t.Fatalf("%d accepted syncs opened %d transactions, want %d", burst, got, want)
				}
				accepted, err := env.svc.Get(ctx, "agent-a")
				if err != nil {
					t.Fatal(err)
				}

				opened = store.txs.Load()
				for i := range refusals {
					res := send(heartbeat(int64(9000+i), func(r *discoverv1.SyncRequest) {
						r.Name = "Refused"
						r.Capabilities.Runners = append(r.Capabilities.Runners, &agentv1.RunnerCapability{Name: "container"})
					}), "token-a")
					if res.outcome != refused {
						t.Fatalf("sync %d over the rate: %s, want refused", i+1, res.outcome)
					}
				}
				if got := store.txs.Load() - opened; got != 0 {
					t.Fatalf("%d refused syncs opened %d transactions, want none", refusals, got)
				}
				stored, err := env.svc.Get(ctx, "agent-a")
				if err != nil {
					t.Fatal(err)
				}
				if stored.Name() != accepted.Name() || stored.UptimeSeconds() != accepted.UptimeSeconds() ||
					len(stored.Capabilities()) != len(accepted.Capabilities()) ||
					!stored.LastSeenAt().Equal(accepted.LastSeenAt()) || !stored.UpdatedAt().Equal(accepted.UpdatedAt()) {
					t.Fatalf("a refused sync changed the row of the agent: it was %q at uptime %d, seen %v, and is %q at uptime %d, seen %v",
						accepted.Name(), accepted.UptimeSeconds(), accepted.LastSeenAt(), stored.Name(), stored.UptimeSeconds(), stored.LastSeenAt())
				}
			})
		}
	}
}

// An agent whose bucket is empty is refused before the token check, whatever
// token the sync presents, so a flood of syncs for it opens no transaction and
// makes no failure for the failure limiter. That shows the throttling state of
// an agent ID to a caller that has not authenticated: a wrong token gets 429
// for an agent that is throttled, where it gets 401 for one that has tokens
// left, and the second takes nothing from the bucket either.
func TestThrottledAgentIsRefusedBeforeTheTokenCheck(t *testing.T) {
	const burst = 10
	for _, tr := range limitTransports {
		t.Run(tr.name, func(t *testing.T) {
			logs := &lockedLogs{}
			store := &txCount{Storage: inmemory.New()}
			env := newLimitEnv(t, store)
			env.logger = zerolog.New(logs)
			env.limiter = discoveryLimiter() // five failures block a pair
			env.enroll(t, map[string]string{"agent-a": "token-a", "agent-b": "token-b"})
			send := tr.syncer(env)

			for i := range burst {
				if res := send(heartbeat(1, nil), "token-a"); res.outcome != synced {
					t.Fatalf("sync %d of the burst: %s, want synced", i+1, res.outcome)
				}
			}

			// agent-a is throttled: every token gets the same answer, and the store is not touched.
			opened := store.txs.Load()
			for i, token := range []string{"stale-token", "", "token-a", "stale-token", "stale-token", "stale-token", "stale-token", "stale-token"} {
				want := syncResult{outcome: refused, message: syncLimited, retryAfter: tr.retryAfter}
				if res := send(heartbeat(1, nil), token); res != want {
					t.Fatalf("sync %d of throttled agent-a with token %q: %+v, want %+v", i+1, token, res, want)
				}
			}
			if got := store.txs.Load() - opened; got != 0 {
				t.Fatalf("syncs of a throttled agent opened %d transactions, want none", got)
			}
			// The WARN of the refusal is the same once a minute for the agent ID, whoever
			// sent the sync, and it names no token.
			if got := logs.levels(t, map[string]string{"agent_id": "agent-a", "message": syncLimited}); len(got) != 1 || got[0] != "warn" {
				t.Fatalf("the refusals logged %v, want one warn", got)
			}
			if strings.Contains(logs.buf.String(), "stale-token") || strings.Contains(logs.buf.String(), "token-a") {
				t.Fatalf("the log holds a token:\n%s", logs.buf.String())
			}

			// agent-b has tokens left: a wrong token is checked, and fails.
			opened = store.txs.Load()
			for i := range 3 {
				if res := send(heartbeat(1, as("agent-b")), "stale-token"); res.outcome != unauthenticated {
					t.Fatalf("wrong token %d for agent-b: %s, want unauthenticated", i+1, res.outcome)
				}
			}
			if got := store.txs.Load() - opened; got != 3 {
				t.Fatalf("three wrong tokens opened %d transactions, want one check each", got)
			}
			// They drained nothing: the burst of agent-b is whole.
			for i := range burst {
				if res := send(heartbeat(1, as("agent-b")), "token-b"); res.outcome != synced {
					t.Fatalf("sync %d of agent-b after the wrong tokens: %s, want synced", i+1, res.outcome)
				}
			}

			// The refusals before the check were no failures of agent-a: once its bucket
			// is back, its token is accepted, though it was refused more than five times.
			env.clock.Advance(10 * time.Second)
			if res := send(heartbeat(1, nil), "token-a"); res.outcome != synced {
				t.Fatalf("agent-a with its token after the refill: %s, want synced", res.outcome)
			}
		})
	}
}

// The charge after the token check is what decides, and the peek before it can
// race a concurrent sync: two syncs of an agent with one token left both pass the
// peek and wait inside their token check, and then one lands and the other is
// refused at the charge, with nothing written for it. The next sync is refused
// at the peek, without a transaction.
func TestSyncThatPassedThePeekMayBeRefusedAtTheCharge(t *testing.T) {
	const burst = 10
	for _, tr := range limitTransports {
		t.Run(tr.name, func(t *testing.T) {
			store := &gatedTx{Storage: inmemory.New(), entered: make(chan struct{}, 8), release: make(chan struct{})}
			env := newLimitEnv(t, store)
			env.enroll(t, map[string]string{"agent-a": "token-a"})
			send := tr.syncer(env)
			for i := range burst - 1 {
				if res := send(heartbeat(1, nil), "token-a"); res.outcome != synced {
					t.Fatalf("sync %d: %s, want synced", i+1, res.outcome)
				}
			}

			// One token is left. Both syncs get past the peek, and wait in the token check.
			store.armed.Store(true)
			results := make(chan syncOutcome, 2)
			for range 2 {
				go func() { results <- send(heartbeat(1, nil), "token-a").outcome }()
			}
			for range 2 {
				select {
				case <-store.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("a sync was refused at the peek, though the bucket had a token")
				}
			}
			close(store.release)
			got := map[syncOutcome]int{}
			for range 2 {
				select {
				case outcome := <-results:
					got[outcome]++
				case <-time.After(5 * time.Second):
					t.Fatal("a sync did not end")
				}
			}
			if got[synced] != 1 || got[refused] != 1 {
				t.Fatalf("two syncs for one token ended %v, want one synced and one refused", got)
			}
			// Two token checks and the write of the winner: the loser wrote nothing.
			if checks := store.checks.Load(); checks != 3 {
				t.Fatalf("the two syncs opened %d transactions, want 3", checks)
			}

			if res := send(heartbeat(1, nil), "token-a"); res.outcome != refused {
				t.Fatalf("sync after the race: %s, want refused", res.outcome)
			}
			if checks := store.checks.Load(); checks != 3 {
				t.Fatalf("the refused sync opened a transaction: %d in all, want 3", checks)
			}
		})
	}
}

// The bucket is charged after the token check, so syncs that present a wrong
// token, or none, take nothing from the bucket of the agent whose ID they
// carry: whoever sends them cannot make that agent's syncs refused.
func TestWrongTokenDoesNotDrainTheBucket(t *testing.T) {
	const burst = 10
	for _, tr := range limitTransports {
		t.Run(tr.name, func(t *testing.T) {
			env := newLimitEnv(t, inmemory.New())
			env.enroll(t, map[string]string{"agent-a": "token-a"})
			send := tr.syncer(env)

			for i := range 3 * burst {
				token := "stale-token"
				if i%3 == 2 {
					token = ""
				}
				if res := send(heartbeat(1, nil), token); res.outcome != unauthenticated {
					t.Fatalf("sync %d with token %q: %s, want unauthenticated", i+1, token, res.outcome)
				}
			}

			for i := range burst {
				if res := send(heartbeat(1, nil), "token-a"); res.outcome != synced {
					t.Fatalf("sync %d of agent-a after the wrong tokens: %s, want synced: they drained the bucket", i+1, res.outcome)
				}
			}
			if res := send(heartbeat(1, nil), "token-a"); res.outcome != refused {
				t.Fatalf("sync %d of agent-a: %s, want refused", burst+1, res.outcome)
			}
		})
	}
}

// A refused sync logs a WARN with the agent ID, at most once a minute for each
// agent ID however long it keeps syncing, and the log never holds its token.
func TestLimitedSyncWarnsOncePerMinutePerAgent(t *testing.T) {
	const burst = 10
	for _, tr := range limitTransports {
		t.Run(tr.name, func(t *testing.T) {
			logs := &lockedLogs{}
			env := newLimitEnv(t, inmemory.New())
			env.logger = zerolog.New(logs)
			env.enroll(t, map[string]string{"agent-a": "token-a", "agent-b": "token-b"})
			send := tr.syncer(env)

			// overspend syncs more than the bucket of the agent holds, and wants all after the burst refused.
			overspend := func(id, token string, refusals int) {
				t.Helper()
				for i := range burst + refusals {
					want := synced
					if i >= burst {
						want = refused
					}
					if res := send(heartbeat(1, as(id)), token); res.outcome != want {
						t.Fatalf("sync %d of %s: %s, want %s", i+1, id, res.outcome, want)
					}
				}
			}
			warns := func(id string) []string {
				return logs.levels(t, map[string]string{"agent_id": id, "message": syncLimited})
			}
			wantWarns := func(when string, a, b int) {
				t.Helper()
				if got := warns("agent-a"); len(got) != a {
					t.Fatalf("%s: %d WARN lines for agent-a, want %d", when, len(got), a)
				}
				if got := warns("agent-b"); len(got) != b {
					t.Fatalf("%s: %d WARN lines for agent-b, want %d", when, len(got), b)
				}
			}

			overspend("agent-a", "token-a", 3)
			overspend("agent-b", "token-b", 2)
			wantWarns("at the first refusals", 1, 1)
			if got := warns("agent-a"); got[0] != "warn" {
				t.Fatalf("the line is at level %q, want warn", got[0])
			}

			env.clock.Advance(30 * time.Second)
			overspend("agent-a", "token-a", 2)
			wantWarns("30 seconds on", 1, 1)

			env.clock.Advance(31 * time.Second)
			overspend("agent-a", "token-a", 1)
			wantWarns("a minute after the first line", 2, 1)

			if strings.Contains(logs.buf.String(), "token-") {
				t.Fatalf("the log holds a token:\n%s", logs.buf.String())
			}
		})
	}
}

// Each capability field of a sync has a limit. Data that breaks one is refused
// with HTTP 400 / InvalidArgument, naming the field, before the token is
// enrolled or anything is written, and it takes nothing from the bucket. The
// limits sit at what the SDK itself lets an agent send, because it treats a
// refusal as permanent; os, arch and platform are cut instead, see
// TestLongPlatformFieldsAreCut.
func TestCapabilityCapsRefuseOversizedFields(t *testing.T) {
	text := func(n int) string { return strings.Repeat("x", n) }
	labels := func(entries int) map[string]string {
		m := make(map[string]string, entries)
		for i := range entries {
			m[fmt.Sprintf("label-%d", i)] = "v"
		}
		return m
	}
	// secondRunner adds the runner that breaks a limit, after the plain one.
	secondRunner := func(edit func(*agentv1.RunnerCapability)) func(*discoverv1.SyncRequest) {
		return func(r *discoverv1.SyncRequest) {
			second := &agentv1.RunnerCapability{Name: "second", Workloads: []*agentv1.WorkloadType{
				{ApiVersion: "solti.io/v1", Kind: "Subprocess"},
				{ApiVersion: "solti.io/v1", Kind: "Subprocess"},
			}}
			edit(second)
			r.Capabilities.Runners = append(r.Capabilities.Runners, second)
		}
	}
	runners := func(n int) []*agentv1.RunnerCapability {
		out := make([]*agentv1.RunnerCapability, n)
		for i := range out {
			out[i] = &agentv1.RunnerCapability{Name: fmt.Sprintf("runner-%d", i)}
		}
		return out
	}
	workloads := func(n int) []*agentv1.WorkloadType {
		out := make([]*agentv1.WorkloadType, n)
		for i := range out {
			out[i] = &agentv1.WorkloadType{ApiVersion: "solti.io/v1", Kind: "Subprocess"}
		}
		return out
	}
	for _, tc := range []struct {
		field string
		edit  func(*discoverv1.SyncRequest)
		rule  string
	}{
		{"runners", func(r *discoverv1.SyncRequest) { r.Capabilities.Runners = runners(65) }, "agent capabilities exceed 64 runners"},
		{"runner name", secondRunner(func(r *agentv1.RunnerCapability) { r.Name = text(129) }), "agent capabilities.runners[1].name exceeds 128 bytes"},
		{"labels", secondRunner(func(r *agentv1.RunnerCapability) { r.Labels = labels(257) }), "agent capabilities.runners[1].labels exceed 256 entries"},
		{"label key", secondRunner(func(r *agentv1.RunnerCapability) { r.Labels = map[string]string{text(318): "v"} }), "agent capabilities.runners[1].labels has a key over 317 bytes"},
		{"label value", secondRunner(func(r *agentv1.RunnerCapability) { r.Labels = map[string]string{"zone": text(1025)} }), "agent capabilities.runners[1].labels has a value over 1024 bytes"},
		{"workloads", secondRunner(func(r *agentv1.RunnerCapability) { r.Workloads = workloads(257) }), "agent capabilities.runners[1].workloads exceed 256 entries"},
		{"workload api_version", secondRunner(func(r *agentv1.RunnerCapability) { r.Workloads[1].ApiVersion = text(318) }), "agent capabilities.runners[1].workloads[1].api_version exceeds 317 bytes"},
		{"workload kind", secondRunner(func(r *agentv1.RunnerCapability) { r.Workloads[1].Kind = text(129) }), "agent capabilities.runners[1].workloads[1].kind exceeds 128 bytes"},
	} {
		for _, tr := range limitTransports {
			t.Run(tr.name+"/"+tc.field, func(t *testing.T) {
				env := newLimitEnv(t, inmemory.New())
				send := tr.syncer(env)

				res := send(heartbeat(1, tc.edit), "token-a")
				if res.outcome != badRequest || res.message != tr.invalid(tc.rule) {
					t.Fatalf("sync with a field over its limit: %s %q, want %s %q", res.outcome, res.message, badRequest, tr.invalid(tc.rule))
				}
				assertNotRegistered(t, env.svc, "agent-a")

				// The refusal drew on no bucket: the burst is whole.
				for i := range 10 {
					if res := send(heartbeat(1, nil), "token-a"); res.outcome != synced {
						t.Fatalf("sync %d of the burst after the refusal: %s, want synced", i+1, res.outcome)
					}
				}
			})
		}
	}
}

// A valid SDK agent is never refused by a cap: the SDK treats a refusal as
// permanent. This sync carries the most the SDK allows in each field (a label
// key and an api_version of a 253-byte DNS prefix, a slash and 63 bytes) and the
// most the caps allow in each count, as far as the 256 KiB body goes, and it is
// stored whole.
func TestCapabilityCapsAcceptWhatTheSDKSends(t *testing.T) {
	text := func(n int) string { return strings.Repeat("x", n) }
	qualified := func(i int) string { return fmt.Sprintf("%0*d", 317, i) } // 317 bytes, distinct
	req := heartbeat(1, func(r *discoverv1.SyncRequest) {
		runners := make([]*agentv1.RunnerCapability, 64)
		for i := range runners {
			runners[i] = &agentv1.RunnerCapability{Name: text(128)}
		}
		runners[0].Labels = make(map[string]string, 256)
		for i := range 256 {
			runners[0].Labels[qualified(i)] = "v"
		}
		runners[1].Workloads = make([]*agentv1.WorkloadType, 256)
		for i := range runners[1].Workloads {
			runners[1].Workloads[i] = &agentv1.WorkloadType{ApiVersion: text(253) + "/" + text(63), Kind: text(128)}
		}
		r.Capabilities.Runners = runners
	})
	for _, tr := range limitTransports {
		t.Run(tr.name, func(t *testing.T) {
			env := newLimitEnv(t, inmemory.New())
			if res := tr.syncer(env)(req, "token-a"); res.outcome != synced {
				t.Fatalf("sync at the limits of the SDK: %s %q, want synced", res.outcome, res.message)
			}
			stored, err := env.svc.Get(context.Background(), "agent-a")
			if err != nil {
				t.Fatal(err)
			}
			caps := stored.Capabilities()
			if len(caps) != 64 || len(caps[0].Labels) != 256 || len(caps[1].Workloads) != 256 {
				t.Fatalf("stored %d runners, %d labels, %d workloads, want 64, 256, 256", len(caps), len(caps[0].Labels), len(caps[1].Workloads))
			}
		})
	}
}

// os, arch and platform are cut to 256 bytes, on a rune boundary, and the sync
// lands: os is the PRETTY_NAME of the host, which the SDK does not bound, and a
// refusal would be permanent there.
func TestLongPlatformFieldsAreCut(t *testing.T) {
	text := func(n int) string { return strings.Repeat("o", n) }
	for _, tc := range []struct{ name, in, want string }{
		{"300 bytes", text(300), text(256)},
		{"a rune that straddles the limit", text(255) + "é" + text(43), text(255)},
	} {
		for _, field := range []string{"os", "arch", "platform"} {
			for _, tr := range limitTransports {
				t.Run(tr.name+"/"+field+", "+tc.name, func(t *testing.T) {
					env := newLimitEnv(t, inmemory.New())
					res := tr.syncer(env)(heartbeat(1, func(r *discoverv1.SyncRequest) {
						switch field {
						case "os":
							r.Os = tc.in
						case "arch":
							r.Arch = tc.in
						case "platform":
							r.Platform = tc.in
						}
					}), "token-a")
					if res.outcome != synced {
						t.Fatalf("sync with a long %s: %s %q, want synced", field, res.outcome, res.message)
					}
					stored, err := env.svc.Get(context.Background(), "agent-a")
					if err != nil {
						t.Fatal(err)
					}
					got := map[string]string{"os": stored.OS(), "arch": stored.Arch(), "platform": stored.Platform()}[field]
					if got != tc.want || !utf8.ValidString(got) {
						t.Fatalf("%s stored as %d bytes, want the first %d bytes of it, on a rune boundary", field, len(got), len(tc.want))
					}
				})
			}
		}
	}
}
