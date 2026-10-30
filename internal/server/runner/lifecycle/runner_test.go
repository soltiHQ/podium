package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster/standalone"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

type heartbeatBeforeTx struct {
	storage.Storage
	fresh *model.Agent
}

func (s *heartbeatBeforeTx) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	_ = s.UpsertAgent(ctx, s.fresh)
	return s.Storage.WithTx(ctx, fn)
}

// txCounter counts the transactions opened through it.
type txCounter struct {
	storage.Storage
	txs atomic.Int64
}

func (s *txCounter) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	s.txs.Add(1)
	return s.Storage.WithTx(ctx, fn)
}

func TestFreshHeartbeatNotOverwrittenByStaleTick(t *testing.T) {
	s := inmemory.New()
	old, _ := model.NewAgentFrom(model.AgentParams{ID: "a", Endpoint: "http://127.0.0.1:8085", EndpointType: 2, APIVersion: 1})
	old.SetLastSeenAt(time.Now().Add(-time.Hour))
	_ = s.UpsertAgent(context.Background(), old)
	fresh := old.Clone()
	fresh.SetLastSeenAt(time.Now())
	r := &Runner{store: &heartbeatBeforeTx{Storage: s, fresh: fresh}, cfg: Config{}.withDefaults(), logger: zerolog.New(io.Discard), hub: event.NewHub(zerolog.New(io.Discard))}
	defer r.hub.Close()
	r.reconcile(context.Background(), time.Now(), old)
	got, err := s.GetAgent(context.Background(), "a")
	if err != nil || got.Status() != enum.AgentStatusActive || !got.LastSeenAt().Equal(fresh.LastSeenAt()) {
		t.Fatal("stale tick deleted or overwrote reconnected agent")
	}
}

func TestLifecycleScansAll503StaleAgents(t *testing.T) {
	s := inmemory.New()
	ctx := context.Background()
	for i := 0; i < 503; i++ {
		a, _ := model.NewAgentFrom(model.AgentParams{ID: fmt.Sprintf("a-%04d", i), Endpoint: "http://127.0.0.1:8085", EndpointType: 2, APIVersion: 1})
		// Silent past the default delete_after (24h), or the tick would keep them.
		a.SetLastSeenAt(time.Now().Add(-25 * time.Hour))
		a.SetStaleAt(time.Now().Add(-25 * time.Hour))
		_ = s.UpsertAgent(ctx, a)
	}
	r := &Runner{store: s, cfg: Config{}.withDefaults(), logger: zerolog.New(io.Discard), hub: event.NewHub(zerolog.New(io.Discard))}
	defer r.hub.Close()
	r.tick(ctx)
	left, err := storage.AllAgents(ctx, s, nil)
	if err != nil || len(left) != 0 {
		t.Fatalf("old stale page skipped %d %v", len(left), err)
	}
}

// fixture runs the lifecycle runner over an in-memory store with a clock the test moves.
type fixture struct {
	store *inmemory.Store
	hub   *event.Hub
	r     *Runner
	now   time.Time
}

// newFixture builds a runner that has no start-up grace.
func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()
	f := &fixture{store: inmemory.New(), hub: event.NewHub(zerolog.New(io.Discard)), now: time.Now()}
	t.Cleanup(f.hub.Close)
	f.r = &Runner{
		store:  f.store,
		cfg:    cfg.withDefaults(),
		logger: zerolog.New(io.Discard),
		hub:    f.hub,
		clock:  func() time.Time { return f.now },
	}
	return f
}

// put stores agent "a" as persisted: last seen at lastSeen, with the heartbeat and status given.
func (f *fixture) put(t *testing.T, lastSeen time.Time, hb time.Duration, status enum.AgentStatus) {
	t.Helper()
	a, err := model.NewAgentFrom(model.AgentParams{ID: "a", Endpoint: "http://127.0.0.1:8085", EndpointType: 2, APIVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	a.SetHeartbeatInterval(hb) // NewAgentFrom refuses a heartbeat over an hour
	a.SetLastSeenAt(lastSeen)
	a.SetStaleAt(lastSeen.Add(hb))
	a.SetStatus(status)
	if err = f.store.UpsertAgent(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

// expect fails unless agent "a" is stored with status, or deleted when gone is set.
func (f *fixture) expect(t *testing.T, when string, status enum.AgentStatus, gone bool) {
	t.Helper()
	a, err := f.store.GetAgent(context.Background(), "a")
	switch {
	case errors.Is(err, storage.ErrNotFound):
		if !gone {
			t.Fatalf("%s: agent deleted, want %s", when, status)
		}
	case err != nil:
		t.Fatalf("%s: %v", when, err)
	case gone:
		t.Fatalf("%s: agent still stored as %s, want deleted", when, a.Status())
	case a.Status() != status:
		t.Fatalf("%s: status = %s, want %s", when, a.Status(), status)
	}
}

// kinds lists the event kinds recorded so far, oldest first.
func (f *fixture) kinds() []string {
	var out []string
	recent := f.hub.RecentEvents(event.RingSize)
	for i := len(recent) - 1; i >= 0; i-- {
		out = append(out, recent[i].Kind)
	}
	return out
}

// expectKinds fails unless exactly the given event kinds were recorded, oldest first.
func (f *fixture) expectKinds(t *testing.T, when string, want []string) {
	t.Helper()
	if got := f.kinds(); !slices.Equal(got, want) {
		t.Fatalf("%s: events = %v, want %v", when, got, want)
	}
}

// step is what must hold a while after the runner started; events lists every kind recorded by then.
type step struct {
	after  time.Duration
	status enum.AgentStatus // ignored once gone
	gone   bool
	events []string
}

// walk moves the clock through the steps, ticks at each and checks agent "a" and the recorded events.
func (f *fixture) walk(t *testing.T, steps []step) {
	t.Helper()
	for _, s := range steps {
		f.now = f.r.startedAt.Add(s.after)
		f.r.tick(context.Background())
		when := fmt.Sprintf("%v after start", s.after)
		f.expect(t, when, s.status, s.gone)
		f.expectKinds(t, when, s.events)
	}
}

// downtimes are how long before the restart the agent was last seen: shorter and longer than delete_after.
var downtimes = []time.Duration{2 * time.Hour, 48 * time.Hour}

func TestRestartAfterLongDowntimeGivesAgentsAGrace(t *testing.T) {
	for _, down := range downtimes {
		t.Run(down.String(), func(t *testing.T) {
			f := newFixture(t, Config{})
			f.r.startedAt = f.now
			// The silence counts from the restart, whenever the agent was last seen.
			f.put(t, f.now.Add(-down), 30*time.Second, enum.AgentStatusActive)
			f.walk(t, []step{
				{10 * time.Second, enum.AgentStatusActive, false, nil},
				{61 * time.Second, enum.AgentStatusInactive, false, []string{event.AgentInactive}},
				{151 * time.Second, enum.AgentStatusDisconnected, false, []string{event.AgentInactive, event.AgentDisconnected}},
				{24*time.Hour + time.Second, 0, true, []string{event.AgentInactive, event.AgentDisconnected, event.AgentDeleted}},
			})
		})
	}
}

// Lifecycle only escalates a status; the restart resets the silence, not the status.
func TestRestartDoesNotDowngradeDisconnectedAgents(t *testing.T) {
	for _, down := range downtimes {
		t.Run(down.String(), func(t *testing.T) {
			f := newFixture(t, Config{})
			f.r.startedAt = f.now
			f.put(t, f.now.Add(-down), 30*time.Second, enum.AgentStatusDisconnected)
			f.walk(t, []step{
				{61 * time.Second, enum.AgentStatusDisconnected, false, nil},
				{151 * time.Second, enum.AgentStatusDisconnected, false, nil},
				{24*time.Hour + time.Second, 0, true, []string{event.AgentDeleted}},
			})
		})
	}
}

func TestDeleteAfterIsAFloor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		hb      time.Duration
		silence time.Duration
		status  enum.AgentStatus // ignored once gone
		gone    bool
	}{
		{"30s heartbeat, 10m is only disconnected", Config{}, 30 * time.Second, 10 * time.Minute, enum.AgentStatusDisconnected, false},
		{"30s heartbeat, exactly 24h is kept", Config{}, 30 * time.Second, 24 * time.Hour, enum.AgentStatusDisconnected, false},
		{"30s heartbeat, past 24h is deleted", Config{}, 30 * time.Second, 24*time.Hour + time.Second, 0, true},
		// 6h heartbeat, delete_multiplier 10: inactive past 12h, disconnected past 30h, deleted past max(24h, 60h).
		{"6h heartbeat, 25h is inactive", Config{}, 6 * time.Hour, 25 * time.Hour, enum.AgentStatusInactive, false},
		{"6h heartbeat, 31h is disconnected: the floor is below 10x", Config{}, 6 * time.Hour, 31 * time.Hour, enum.AgentStatusDisconnected, false},
		{"6h heartbeat, past 60h is deleted", Config{}, 6 * time.Hour, 60*time.Hour + time.Second, 0, true},
		{"delete_after 48h, 25h is kept", Config{DeleteAfter: 48 * time.Hour}, 30 * time.Second, 25 * time.Hour, enum.AgentStatusDisconnected, false},
		{"delete_after 48h, past 48h is deleted", Config{DeleteAfter: 48 * time.Hour}, 30 * time.Second, 48*time.Hour + time.Second, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.cfg)
			f.put(t, f.now.Add(-tc.silence), tc.hb, enum.AgentStatusActive)
			f.r.tick(context.Background())
			f.expect(t, "after a tick", tc.status, tc.gone)
		})
	}
}

// Long after the start, the silence must still run from the agent's last sync, not from the start.
func TestSteadyStateSilenceRunsFromTheLastSync(t *testing.T) {
	for _, tc := range []struct {
		name    string
		silence time.Duration
		status  enum.AgentStatus
		events  []string
	}{
		{"31s is still active", 31 * time.Second, enum.AgentStatusActive, nil},
		{"61s is inactive", 61 * time.Second, enum.AgentStatusInactive, []string{event.AgentInactive}},
		{"10m is disconnected, not deleted", 10 * time.Minute, enum.AgentStatusDisconnected, []string{event.AgentDisconnected}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, Config{})
			f.r.startedAt = f.now.Add(-72 * time.Hour)
			f.put(t, f.now.Add(-tc.silence), 30*time.Second, enum.AgentStatusActive)
			f.r.tick(context.Background())
			f.expect(t, "after a tick", tc.status, false)
			f.expectKinds(t, "after a tick", tc.events)
		})
	}
}

// A scanned agent with nothing due must not open a transaction: on the durable store each one clones, marshals and
// fsyncs the whole state under the lock that sync's Upsert shares.
func TestTickOpensNoTransactionForAgentsWithNothingDue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		up        time.Duration // how long the runner has been up; 0 = no grace
		silence   time.Duration
		persisted enum.AgentStatus
		txs       int64
	}{
		{"one heartbeat missed", 0, 31 * time.Second, enum.AgentStatusActive, 0},
		{"already inactive, not yet disconnected", 0, 100 * time.Second, enum.AgentStatusInactive, 0},
		{"already disconnected, below the delete threshold", 0, 10 * time.Minute, enum.AgentStatusDisconnected, 0},
		{"inside the restart grace", 10 * time.Second, 2 * time.Hour, enum.AgentStatusActive, 0},
		{"inactive is due", 0, 61 * time.Second, enum.AgentStatusActive, 1},
		{"deletion is due", 0, 25 * time.Hour, enum.AgentStatusDisconnected, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, Config{})
			counter := &txCounter{Storage: f.store}
			f.r.store = counter
			if tc.up > 0 {
				f.r.startedAt = f.now.Add(-tc.up)
			}
			f.put(t, f.now.Add(-tc.silence), 30*time.Second, tc.persisted)
			f.r.tick(context.Background())
			if got := counter.txs.Load(); got != tc.txs {
				t.Fatalf("transactions = %d, want %d", got, tc.txs)
			}
		})
	}
}

// The decision is made again on the re-read agent: a status that moved on since the scan is never lowered.
func TestReconcileDecidesAgainOnTheReReadAgent(t *testing.T) {
	f := newFixture(t, Config{})
	ctx := context.Background()
	f.put(t, f.now.Add(-61*time.Second), 30*time.Second, enum.AgentStatusActive) // the scan sees inactive due
	scanned, err := f.store.GetAgent(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	moved := scanned.Clone()
	moved.SetStatus(enum.AgentStatusDisconnected)
	f.r.store = &heartbeatBeforeTx{Storage: f.store, fresh: moved}
	f.r.reconcile(ctx, f.now, scanned)
	f.expect(t, "after reconcile", enum.AgentStatusDisconnected, false)
	f.expectKinds(t, "after reconcile", nil)
}

// The grace exists only if New starts it: the tests above set startedAt by hand.
func TestNewRecordsStartedAt(t *testing.T) {
	hub := event.NewHub(zerolog.New(io.Discard))
	defer hub.Close()
	before := time.Now()
	r, err := New(Config{}, zerolog.New(io.Discard), inmemory.New(), hub, standalone.NewLeadership())
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if r.startedAt.Before(before) || r.startedAt.After(after) {
		t.Fatalf("startedAt = %v, want between %v and %v", r.startedAt, before, after)
	}
}
