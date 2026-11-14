package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// The tests of the events transport run on Postgres, each on a database of its
// own from pgtest, and skip without one, but for the cases of
// TestWakerErrorsNeverCarryURL that need no server. They wait on conditions:
// the intervals that would make them wait are knobs they shorten, or lengthen
// past their deadlines, so that only the path under test can pass them.

// eventsWithin is how long a test waits for events to reach a hub.
const eventsWithin = 10 * time.Second

// testRecord is the kind of the records the tests send; their payload's ID
// tells them apart.
const testRecord = "test_record"

// follower is a cluster.Leadership that never leads: a transport on it never
// prunes.
type follower struct{}

func (follower) AmLeader() bool        { return false }
func (follower) CurrentLeader() string { return "" }
func (follower) WhenLeader(ctx context.Context, _ func(context.Context) error) error {
	<-ctx.Done()
	return nil
}

// eventNodeConfig is what a test transport differs in; a zero field keeps the
// default.
type eventNodeConfig struct {
	// listen gives the transport a Postgres waker; without one it polls.
	listen bool
	// backoff is the waker's first pause before it reconnects.
	backoff    time.Duration
	readEvery  time.Duration
	pruneEvery time.Duration
	timeout    time.Duration
	// lead is the transport's leadership; nil: follower.
	lead cluster.Leadership
}

// eventNode is an events transport under test, on a pool of its own, with its
// hub, the pulses its hub's client got, and its log lines.
type eventNode struct {
	*EventTransport
	name   string
	hub    *event.Hub
	pulses *pulseLog
	logs   *logBuffer
}

// newEventNode builds the transport name on the database at databaseURL,
// without running it.
func newEventNode(t *testing.T, databaseURL, name string, cfg eventNodeConfig) *eventNode {
	t.Helper()
	var w Waker
	if cfg.listen {
		pw := NewPostgresWaker(databaseURL, EventsChannel).(*pgWaker)
		if cfg.backoff > 0 {
			pw.backoffMin, pw.backoffMax = cfg.backoff, max(pw.backoffMax, cfg.backoff)
		}
		w = pw
	}
	lead := cfg.lead
	if lead == nil {
		lead = follower{}
	}
	hub := event.NewHub(zerolog.Nop())
	t.Cleanup(hub.Close)
	logs := &logBuffer{}
	tr := NewEventTransport(pgtest.Open(t, databaseURL), sqlstore.Postgres{}, w, hub, lead, zerolog.New(logs))
	if cfg.readEvery > 0 {
		tr.readEvery = cfg.readEvery
	}
	if cfg.pruneEvery > 0 {
		tr.pruneEvery = cfg.pruneEvery
	}
	if cfg.timeout > 0 {
		tr.timeout = cfg.timeout
	}
	return &eventNode{EventTransport: tr, name: name, hub: hub, pulses: subscribePulses(t, hub), logs: logs}
}

// start runs the transport until the test's end, which checks that Run
// returns nil.
func (n *eventNode) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("transport %s: Run: %v", n.name, err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("transport %s: Run did not return within 10 s of its context's end", n.name)
		}
	})
}

// ready waits until the transport applies an event of its own: Run knows where
// it starts, and reads.
func (n *eventNode) ready(t *testing.T) {
	t.Helper()
	n.Record("ready", event.Payload{ID: n.name})
	waitFor(t, eventsWithin, n.name+" applying its own event", func() bool { return slices.Contains(records(n.hub, "ready"), n.name) })
}

// pulseLog collects, in order, the pulses a client of a hub gets.
type pulseLog struct {
	mu  sync.Mutex
	got []string
}

// subscribePulses subscribes a client to hub until the test's end. The hub
// drops a pulse for a client whose buffer of 16 is full: a test that counts on
// every pulse sends 16 at most to a hub.
func subscribePulses(t *testing.T, hub *event.Hub) *pulseLog {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := hub.Subscribe(ctx)
	p := &pulseLog{}
	go func() {
		for name := range ch {
			p.mu.Lock()
			p.got = append(p.got, name)
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *pulseLog) list() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.got)
}

// records returns the payload IDs of the records of kind that hub applied,
// oldest first.
func records(hub *event.Hub, kind string) []string {
	recent := hub.RecentEvents(event.RingSize)
	var ids []string
	for i := len(recent) - 1; i >= 0; i-- {
		if recent[i].Kind == kind {
			ids = append(ids, recent[i].Payload.ID)
		}
	}
	return ids
}

// hasIssue reports whether hub holds an issue of kind about the entity id.
func hasIssue(hub *event.Hub, kind, id string) bool {
	for _, rec := range hub.RecentIssues(event.RingSize) {
		if rec.Kind == kind && rec.Payload.ID == id {
			return true
		}
	}
	return false
}

// mustEncode is the payload of op, as the transport writes it.
func mustEncode(t *testing.T, op *raftv1.Op) []byte {
	t.Helper()
	payload, err := encode(op)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return payload
}

// rowQuerier is what insertRow writes through: the test's pool, or a
// transaction of the test's.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// insertRow writes an event row through q as another replica's transport
// would, and returns its sequence.
func insertRow(t *testing.T, q rowQuerier, kind string, payload []byte) int64 {
	t.Helper()
	var seq int64
	if err := q.QueryRowContext(t.Context(), `INSERT INTO podium_events (kind, payload) VALUES ($1, $2) RETURNING seq`, kind, payload).Scan(&seq); err != nil {
		t.Fatalf("insert an event row: %v", err)
	}
	return seq
}

// insertAged writes an event row created age ago, by the database's clock, and
// returns its sequence.
func insertAged(t *testing.T, admin *sql.DB, age string, payload []byte) int64 {
	t.Helper()
	var seq int64
	err := admin.QueryRowContext(t.Context(), `INSERT INTO podium_events (kind, payload, created_at)
VALUES ($1, $2, now() - $3::interval) RETURNING seq`, kindRecord, payload, age).Scan(&seq)
	if err != nil {
		t.Fatalf("insert an event row %s old: %v", age, err)
	}
	return seq
}

// rowExists reports whether the event row seq is there.
func rowExists(t *testing.T, admin *sql.DB, seq int64) bool {
	t.Helper()
	var n int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM podium_events WHERE seq = $1`, seq).Scan(&n); err != nil {
		t.Fatalf("read the event row %d: %v", seq, err)
	}
	return n == 1
}

// storedRow is an event row as a test reads it back, decoded.
type storedRow struct {
	seq int64
	op  *raftv1.Op
}

// storedRows reads every event row of kind, in sequence, decoded.
func storedRows(t *testing.T, admin *sql.DB, kind string) []storedRow {
	t.Helper()
	rows, err := admin.QueryContext(t.Context(), `SELECT seq, payload FROM podium_events WHERE kind = $1 ORDER BY seq`, kind)
	if err != nil {
		t.Fatalf("read the event rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []storedRow
	for rows.Next() {
		var (
			r       storedRow
			payload []byte
		)
		if err := rows.Scan(&r.seq, &payload); err != nil {
			t.Fatalf("read an event row: %v", err)
		}
		op, ok := decode(kind, payload)
		if !ok {
			t.Fatalf("the event row %d does not decode", r.seq)
		}
		r.op = op
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the event rows: %v", err)
	}
	return out
}

// storedPulses returns the pulses the rows hold, in sequence.
func storedPulses(t *testing.T, admin *sql.DB) []string {
	t.Helper()
	var names []string
	for _, r := range storedRows(t, admin, kindNotify) {
		names = append(names, r.op.GetEventNotify())
	}
	return names
}

// storedRecords returns the payload IDs of the records of kind the rows hold, in
// sequence, with the sequence of each by ID.
func storedRecords(t *testing.T, admin *sql.DB, kind string) ([]string, map[string]int64) {
	t.Helper()
	var ids []string
	seqs := map[string]int64{}
	for _, r := range storedRows(t, admin, kindRecord) {
		if rec := r.op.GetEventRecord(); rec.GetKind() == kind {
			ids = append(ids, rec.GetPayload().GetId())
			seqs[rec.GetPayload().GetId()] = r.seq
		}
	}
	return ids, seqs
}

// holdEventLock takes the dialect's event lock in a transaction of the test's,
// as another replica's write would, and returns the transaction, which the
// test's end rolls back.
func holdEventLock(t *testing.T, admin *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(t.Context(), sqlstore.Postgres{}.EventLockSQL()); err != nil {
		t.Fatalf("take the event lock: %v", err)
	}
	return tx
}

// listeners returns the sessions on the test's database of the Postgres waker.
func listeners(t *testing.T, admin *sql.DB) int {
	t.Helper()
	var n int
	err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity
 WHERE datname = current_database() AND application_name = $1`, listenerName).Scan(&n)
	if err != nil {
		t.Fatalf("count the listeners: %v", err)
	}
	return n
}

// checkNoSecrets fails t if text, a transport's log lines, carries a database
// URL, the test server's host or its password. It never prints text.
func checkNoSecrets(t *testing.T, what, text string) {
	t.Helper()
	server, err := url.Parse(os.Getenv(pgtest.EnvURL))
	if err != nil {
		t.Fatalf("%s does not parse", pgtest.EnvURL)
	}
	secrets := map[string]string{"a postgres:// URL": "postgres://", "the server's host": server.Host}
	if password, ok := server.User.Password(); ok {
		secrets["the password"] = password
	}
	for name, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Errorf("%s carry %s", what, name)
		}
	}
}

// TestEventsReachEveryReplica runs two transports on one database, each with a
// listener and no reads on an interval, so that every row reaches a replica
// through a wake-up, and sends pulses and records from several goroutines on
// both at once: each hub applies every event once, the writer's own included, in
// the order of the rows' sequences, which is the other hub's order too, and
// each goroutine's events in the order it sent them.
func TestEventsReachEveryReplica(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	nodes := []*eventNode{
		newEventNode(t, databaseURL, "a", eventNodeConfig{listen: true, readEvery: time.Hour}),
		newEventNode(t, databaseURL, "b", eventNodeConfig{listen: true, readEvery: time.Hour}),
	}
	for _, n := range nodes {
		n.start(t)
	}
	for _, n := range nodes {
		n.ready(t)
	}

	// Every third event a pulse: 16 pulses in all, which a client's buffer holds.
	const goroutines, perGoroutine = 4, 6
	const pulses, recordCount = 2 * goroutines * 2, 2 * goroutines * 4
	var wg sync.WaitGroup
	for _, n := range nodes {
		for g := range goroutines {
			wg.Go(func() {
				for i := range perGoroutine {
					id := fmt.Sprintf("%s-%d-%d", n.name, g, i)
					if i%3 == 0 {
						n.Notify("pulse-" + id)
					} else {
						n.Record(testRecord, event.Payload{ID: id})
					}
				}
			})
		}
	}
	wg.Wait()
	for _, n := range nodes {
		waitFor(t, eventsWithin, n.name+" applying every event", func() bool {
			return len(n.pulses.list()) >= pulses && len(records(n.hub, testRecord)) >= recordCount
		})
	}
	// A last record of each, after which no event may come twice.
	for _, n := range nodes {
		n.Record("last", event.Payload{ID: n.name})
	}
	for _, n := range nodes {
		waitFor(t, eventsWithin, n.name+" applying both last records", func() bool { return len(records(n.hub, "last")) == 2 })
	}

	wantPulses := storedPulses(t, admin)
	wantRecords, _ := storedRecords(t, admin, testRecord)
	if len(wantPulses) != pulses || len(wantRecords) != recordCount {
		t.Fatalf("the rows hold %d pulses and %d records, want %d and %d", len(wantPulses), len(wantRecords), pulses, recordCount)
	}
	for _, n := range nodes {
		if got := n.pulses.list(); !slices.Equal(got, wantPulses) {
			t.Errorf("%s applied the pulses %q, want each once in the rows' order %q", n.name, got, wantPulses)
		}
		if got := records(n.hub, testRecord); !slices.Equal(got, wantRecords) {
			t.Errorf("%s applied the records %q, want each once in the rows' order %q", n.name, got, wantRecords)
		}
	}
	// Each goroutine's events are in the order it sent them.
	for _, ids := range [][]string{wantRecords, wantPulses} {
		next := map[string]int{}
		for _, id := range ids {
			var (
				node    string
				g, i    int
				trimmed = strings.TrimPrefix(id, "pulse-")
			)
			if _, err := fmt.Sscanf(strings.ReplaceAll(trimmed, "-", " "), "%s %d %d", &node, &g, &i); err != nil {
				t.Fatalf("event %q: %v", id, err)
			}
			key := fmt.Sprintf("%s-%d", node, g)
			if i < next[key] {
				t.Errorf("the events of goroutine %s are out of order: %q", key, ids)
				break
			}
			next[key] = i + 1
		}
	}
}

// TestEventsResumeAfterReconnect ends a transport's listener while another
// transport writes events. The reads on an interval are off and the listener
// waits two seconds before it reconnects, so the events are written while it is
// down, and only its reconnect, which counts as a wake-up, can deliver them: it
// applies them, in order, once it is back.
func TestEventsResumeAfterReconnect(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	writer := newEventNode(t, databaseURL, "writer", eventNodeConfig{})
	listener := newEventNode(t, databaseURL, "listener", eventNodeConfig{listen: true, backoff: 2 * time.Second, readEvery: time.Hour})
	writer.start(t)
	listener.start(t)
	writer.ready(t)
	listener.ready(t)
	waitFor(t, eventsWithin, "the listener connected", func() bool { return listeners(t, admin) == 1 })

	// The listener's session ends before the events are written.
	if _, err := admin.ExecContext(t.Context(), `SELECT pg_terminate_backend(pid, 5000) FROM pg_stat_activity
 WHERE datname = current_database() AND application_name = $1`, listenerName); err != nil {
		t.Fatalf("end the listener's session: %v", err)
	}
	want := []string{"while-down-0", "while-down-1", "while-down-2"}
	for _, id := range want {
		writer.Record(testRecord, event.Payload{ID: id})
	}
	waitFor(t, eventsWithin, "the writer applying its events", func() bool { return slices.Equal(records(writer.hub, testRecord), want) })
	if n := listeners(t, admin); n != 0 {
		t.Fatalf("the listener was back before the events were written (%d sessions): its backoff is too short for this test", n)
	}
	if got := records(listener.hub, testRecord); len(got) != 0 {
		t.Fatalf("the listener applied %q while it was down", got)
	}

	waitFor(t, eventsWithin, "the listener applying the events once it reconnected", func() bool {
		return slices.Equal(records(listener.hub, testRecord), want)
	})
	if n := listeners(t, admin); n != 1 {
		t.Errorf("the listener has %d sessions once it applied the events, want its new one", n)
	}
	logs := listener.logs.String()
	if !strings.Contains(logs, "the events listener failed") {
		t.Error("the listener's failure was not logged")
	}
	checkNoSecrets(t, "the listener's log lines", logs)
}

// TestEventQueueBounded stalls the transport's writes with a lock on
// podium_events, which reads pass: Notify, Record and DeleteIssues never block;
// the queue holds queueSize events behind the write in flight; every event past
// them is applied on this replica alone, counted and logged. Once the lock goes,
// the stalled write and the queued events are written, in order, and none of
// them counts as dropped.
func TestEventQueueBounded(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	n := newEventNode(t, databaseURL, "a", eventNodeConfig{timeout: time.Minute})
	n.start(t)
	n.ready(t)

	lock, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Rollback() })
	if _, err := lock.ExecContext(t.Context(), `LOCK TABLE podium_events IN EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock podium_events: %v", err)
	}
	n.Notify("stalled")
	waitFor(t, eventsWithin, "the write waiting for the lock", func() bool { return lockWaiters(t, admin) == 1 })

	const past = 10
	var deleted int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range queueSize + past {
			n.Notify(fmt.Sprintf("queued-%04d", i))
		}
		n.Record(testRecord, event.Payload{ID: "past-the-queue"})
		n.hub.ApplyLocalRecord(event.AgentInactive, event.Payload{ID: "agent-1"})
		deleted = n.DeleteIssues(event.AgentInactive, "agent-1")
	}()
	receive(t, done, 5*time.Second, "the calls while the database is stalled")
	if got := n.Dropped(); got != past+2 {
		t.Errorf("dropped %d events, want the %d past the queue", got, past+2)
	}
	if got := records(n.hub, testRecord); !slices.Equal(got, []string{"past-the-queue"}) {
		t.Errorf("the records applied here: %q, want the one past the queue, at once", got)
	}
	if deleted != 1 || hasIssue(n.hub, event.AgentInactive, "agent-1") {
		t.Errorf("DeleteIssues past the queue removed %d issues, the issue left %v; want it removed here, 1", deleted, hasIssue(n.hub, event.AgentInactive, "agent-1"))
	}
	if !strings.Contains(n.logs.String(), "the event queue is full") {
		t.Error("the overflow was not logged")
	}

	_ = lock.Rollback()
	want := []string{"stalled"}
	for i := range queueSize {
		want = append(want, fmt.Sprintf("queued-%04d", i))
	}
	waitFor(t, eventsWithin, "the stalled and the queued events written", func() bool { return len(storedPulses(t, admin)) >= len(want) })
	if got := storedPulses(t, admin); !slices.Equal(got, want) {
		t.Errorf("the rows hold %d pulses, want the stalled one and the %d queued, in order", len(got), queueSize)
	}
	if got := n.Dropped(); got != past+2 {
		t.Errorf("dropped %d events once the queue drained, want still %d", got, past+2)
	}
}

// TestEventWritesSerialized holds the dialect's event lock in a transaction of
// the test's, as another replica's write in flight would: the transport's write
// waits for it before it takes a sequence, so its row comes after the row the
// holder writes, though the holder writes it later, and the transport applies
// both in that order.
func TestEventWritesSerialized(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	n := newEventNode(t, databaseURL, "a", eventNodeConfig{timeout: time.Minute})
	n.start(t)
	n.ready(t)

	holder := holdEventLock(t, admin)
	n.Record(testRecord, event.Payload{ID: "waits"})
	waitFor(t, eventsWithin, "the write waiting for the event lock", func() bool { return lockWaiters(t, admin) == 1 })
	if ids, _ := storedRecords(t, admin, testRecord); len(ids) != 0 {
		t.Fatalf("the write waiting for the lock wrote %q", ids)
	}
	held := insertRow(t, holder, kindRecord, mustEncode(t, recordOp(testRecord, event.Payload{ID: "holder"})))
	if err := holder.Commit(); err != nil {
		t.Fatalf("commit the holder's row: %v", err)
	}

	waitFor(t, eventsWithin, "the transport applying both rows", func() bool { return len(records(n.hub, testRecord)) == 2 })
	if got := records(n.hub, testRecord); !slices.Equal(got, []string{"holder", "waits"}) {
		t.Errorf("applied %q, want the holder's row, then the one that waited", got)
	}
	if _, seqs := storedRecords(t, admin, testRecord); seqs["waits"] <= held {
		t.Errorf("the row that waited has the sequence %d, want one after the holder's, %d", seqs["waits"], held)
	}
}

// TestDeleteIssuesCountsLocally checks DeleteIssues: it removes the matching
// issues on its replica at the call and returns how many, and the other replica
// removes them once it reads the deletion's row. The writer applies its own row
// again in sequence, which removes an issue whose row came before the
// deletion's though it reached the writer only after the call, as on every
// other replica.
func TestDeleteIssuesCountsLocally(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	a := newEventNode(t, databaseURL, "a", eventNodeConfig{listen: true})
	// b reads after its own writes alone: no listener, no reads on an interval.
	b := newEventNode(t, databaseURL, "b", eventNodeConfig{readEvery: time.Hour})
	a.start(t)
	b.start(t)
	a.ready(t)
	b.ready(t)

	a.Record(event.AgentInactive, event.Payload{ID: "agent-1"})
	waitFor(t, eventsWithin, "a holding the issue", func() bool { return hasIssue(a.hub, event.AgentInactive, "agent-1") })
	b.Record(testRecord, event.Payload{ID: "b-reads"})
	waitFor(t, eventsWithin, "b holding the issue", func() bool { return hasIssue(b.hub, event.AgentInactive, "agent-1") })

	if got := b.DeleteIssues(event.AgentInactive, "agent-1"); got != 1 {
		t.Errorf("DeleteIssues = %d, want the 1 issue b held", got)
	}
	if hasIssue(b.hub, event.AgentInactive, "agent-1") {
		t.Error("DeleteIssues left the issue on its replica: want it removed at the call")
	}
	waitFor(t, eventsWithin, "a removing the issue once it read the deletion's row", func() bool {
		return !hasIssue(a.hub, event.AgentInactive, "agent-1")
	})

	// Another replica's issue that commits after b's call, but takes its
	// sequence before b's deletion, which waits for its lock.
	holder := holdEventLock(t, admin)
	if got := b.DeleteIssues(event.AgentInactive, "agent-2"); got != 0 {
		t.Errorf("DeleteIssues of an issue no replica holds yet = %d, want 0", got)
	}
	waitFor(t, eventsWithin, "b's deletion waiting for the event lock", func() bool { return lockWaiters(t, admin) == 1 })
	insertRow(t, holder, kindRecord, mustEncode(t, recordOp(event.AgentInactive, event.Payload{ID: "agent-2"})))
	if err := holder.Commit(); err != nil {
		t.Fatalf("commit the issue's row: %v", err)
	}
	b.Record(testRecord, event.Payload{ID: "after-the-deletion"})
	waitFor(t, eventsWithin, "b applying its record after the deletion", func() bool {
		return slices.Contains(records(b.hub, testRecord), "after-the-deletion")
	})
	if !slices.Contains(records(b.hub, event.AgentInactive), "agent-2") {
		t.Error("b skipped the issue's row")
	}
	if hasIssue(b.hub, event.AgentInactive, "agent-2") {
		t.Error("b holds the issue whose row came before its deletion's: want its own deletion applied again in sequence")
	}
	waitFor(t, eventsWithin, "a applying the issue and the deletion", func() bool {
		return slices.Contains(records(a.hub, testRecord), "after-the-deletion")
	})
	if hasIssue(a.hub, event.AgentInactive, "agent-2") {
		t.Error("a holds the issue whose row came before b's deletion")
	}
}

// TestRunStartsAfterTheNewestRow checks where a transport starts: after the
// newest row present when Run starts, so that the rows before are never applied
// (the hub's history stays transient), while an event sent before Run is
// written once Run knows where it starts, and applied.
func TestRunStartsAfterTheNewestRow(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	for _, id := range []string{"history-0", "history-1"} {
		insertRow(t, admin, kindRecord, mustEncode(t, recordOp(testRecord, event.Payload{ID: id})))
	}
	n := newEventNode(t, databaseURL, "a", eventNodeConfig{})
	n.Record(testRecord, event.Payload{ID: "before-run"})
	n.start(t)
	waitFor(t, eventsWithin, "the event sent before Run applied", func() bool {
		return slices.Contains(records(n.hub, testRecord), "before-run")
	})
	if got := records(n.hub, testRecord); !slices.Equal(got, []string{"before-run"}) {
		t.Errorf("applied %q, want the event sent before Run alone, not the rows there before it", got)
	}
}

// TestUnreadableRowsSkipped writes rows the transport cannot read, as a newer
// podium might during a rolling upgrade: a row of an unknown kind, a payload that
// does not decode, and an op other than its row's kind. The transport skips
// each, logs it without its payload, and reads on: a payload with fields it does
// not know is read, the fields dropped.
func TestUnreadableRowsSkipped(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	n := newEventNode(t, databaseURL, "a", eventNodeConfig{readEvery: 20 * time.Millisecond})
	n.start(t)
	n.ready(t)

	const secret = "payload-secret-1729"
	insertRow(t, admin, "from_a_newer_podium", []byte(`{"eventRecord":{"kind":"`+secret+`"}}`))
	insertRow(t, admin, kindRecord, []byte(`{"eventRecord":`+secret))
	insertRow(t, admin, kindNotify, mustEncode(t, recordOp(secret, event.Payload{ID: secret})))
	insertRow(t, admin, kindRecord, []byte(`{"eventRecord":{"kind":"`+testRecord+`","payload":{"id":"tolerated","fromTheFuture":"`+secret+`"}},"alsoNew":1}`))
	n.Record(testRecord, event.Payload{ID: "after"})

	waitFor(t, eventsWithin, "the event after the unreadable rows applied", func() bool {
		return slices.Contains(records(n.hub, testRecord), "after")
	})
	if got := records(n.hub, testRecord); !slices.Equal(got, []string{"tolerated", "after"}) {
		t.Errorf("applied %q, want the tolerated record and the one after", got)
	}
	for _, rec := range n.hub.RecentEvents(event.RingSize) {
		if rec.Kind == secret || rec.Payload.ID == secret {
			t.Error("the transport applied an op other than its row's kind")
		}
	}
	logs := n.logs.String()
	if !strings.Contains(logs, "skipped an event row") {
		t.Error("the skipped rows were not logged")
	}
	if strings.Contains(logs, secret) {
		t.Error("a log line carries a skipped row's payload")
	}
}

// TestEventsPruned checks that the leader alone prunes the rows older than ten
// minutes, by the database's clock: a transport on a follower keeps them, and
// the same transport prunes them once its replica leads, keeping the younger
// rows.
func TestEventsPruned(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{poll: 50 * time.Millisecond})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)
	b := newReplica(t, databaseURL, "replica-b", replicaConfig{poll: 50 * time.Millisecond})
	b.start(t)

	payload := mustEncode(t, recordOp(testRecord, event.Payload{ID: "old"}))
	old := []int64{insertAged(t, admin, "11 minutes", payload), insertAged(t, admin, "1 hour", payload)}
	young := insertAged(t, admin, "9 minutes", payload)

	const pruneEvery = 10 * time.Millisecond
	n := newEventNode(t, databaseURL, "b", eventNodeConfig{lead: b.Leadership, pruneEvery: pruneEvery})
	started := time.Now()
	n.start(t)
	n.ready(t)
	// A follower's transport has twenty prune intervals to prune, and must not:
	// the only wait a check of something that does not happen can make.
	waitFor(t, 5*time.Second, "twenty prune intervals", func() bool { return time.Since(started) >= 20*pruneEvery })
	if b.AmLeader() {
		t.Fatal("replica-b leads before replica-a stopped")
	}
	for _, seq := range old {
		if !rowExists(t, admin, seq) {
			t.Fatalf("a follower's transport pruned the row %d", seq)
		}
	}

	if err := a.stop(); err != nil {
		t.Fatalf("replica-a's Run: %v", err)
	}
	waitFor(t, 5*time.Second, "replica-b leading", b.AmLeader)
	waitFor(t, 5*time.Second, "the rows older than ten minutes pruned", func() bool {
		return !rowExists(t, admin, old[0]) && !rowExists(t, admin, old[1])
	})
	if !rowExists(t, admin, young) {
		t.Error("the leader pruned a row nine minutes old")
	}
	if ids, _ := storedRecords(t, admin, "ready"); !slices.Equal(ids, []string{"b"}) {
		t.Errorf("the rows of ready records left: %q, want the transport's own", ids)
	}
}

// TestWakerErrorsNeverCarryURL has the Postgres waker connect with URLs that
// carry a user and a password: a host that does not resolve, a port nothing
// listens on, a URL that does not parse, and, with a server to test against, a
// wrong password. Wait fails with an error that names neither the URL nor its
// user, host or password, and the failed connections are storage.ErrUnavailable,
// a refusal naming its SQLSTATE.
func TestWakerErrorsNeverCarryURL(t *testing.T) {
	const (
		user     = "podium-leak-user"
		password = "leak-password-1729"
	)
	type leak struct {
		name, url, host string
		unavailable     bool
		sqlstate        string
	}
	cases := []leak{
		{"a host that does not resolve", "postgres://" + user + ":" + password + "@podium-leak-host.invalid:5432/podium?connect_timeout=5", "podium-leak-host", true, ""},
		{"a port nothing listens on", "postgres://" + user + ":" + password + "@127.0.0.1:1/podium?connect_timeout=5", "127.0.0.1", true, ""},
		{"a URL that does not parse", "postgres://" + user + ":" + password + "@podium-leak-host:port/podium", "podium-leak-host", false, ""},
	}
	if server := os.Getenv(pgtest.EnvURL); server != "" {
		u, err := url.Parse(server)
		if err != nil {
			t.Fatalf("%s does not parse", pgtest.EnvURL)
		}
		u.User = url.UserPassword(user, password)
		cases = append(cases, leak{"a wrong password", u.String(), u.Hostname(), true, "28P01"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			err := NewPostgresWaker(tc.url, EventsChannel).Wait(ctx)
			switch {
			case err == nil:
				t.Fatal("Wait answered a wake-up")
			case ctx.Err() != nil:
				t.Fatalf("Wait did not fail within the test's deadline: %v", err)
			case tc.unavailable && !errors.Is(err, storage.ErrUnavailable):
				t.Errorf("Wait: %v, want storage.ErrUnavailable", err)
			case tc.sqlstate != "" && !strings.Contains(err.Error(), "SQLSTATE "+tc.sqlstate):
				t.Errorf("Wait: %q, want it to name SQLSTATE %s", err, tc.sqlstate)
			}
			for what, secret := range map[string]string{
				"the password": password, "the user": user, "the host": tc.host, "a postgres:// URL": "postgres://",
			} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("Wait: the error carries %s", what)
				}
			}
		})
	}
}
