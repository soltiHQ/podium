package database

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/encoding/protojson"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
)

var _ event.Writer = (*EventTransport)(nil)

// EventsChannel is the channel the transport's wake-ups go out on, and the
// listener of every replica, NewPostgresWaker's, listens on.
const EventsChannel = "podium_events"

// The kinds of the rows of podium_events: constants of podium's own, never the
// kind of an event, which the payload carries. A replica skips a row of a kind
// it does not know.
const (
	kindNotify       = "notify"
	kindRecord       = "record"
	kindDeleteIssues = "delete_issues"
)

const (
	// queueSize is how many events wait for the writer at most: an event past
	// them is applied on its replica alone.
	queueSize = 1024
	// writeBatch is how many queued events one transaction writes at most.
	writeBatch = 128
	// readBatch is how many rows one read returns at most.
	readBatch = 1000
)

// The transport's intervals. An EventTransport keeps each in a field, which
// tests change before Run.
const (
	// eventsTimeout bounds a write of events, a read and a prune.
	eventsTimeout = 5 * time.Second
	// listenedReadEvery is how often a transport with a listener reads without a
	// wake-up: a listener that died without a word delays events that long.
	listenedReadEvery = 5 * time.Second
	// polledReadEvery is how often a transport without a listener reads.
	polledReadEvery = time.Second
	// eventsPruneEvery is how often the leader prunes the rows.
	eventsPruneEvery = time.Minute
	// eventsPruneAge is how old a row gets, by the database's clock, before the
	// leader prunes it.
	eventsPruneAge = 10 * time.Minute
	// eventsLogEvery is how often one kind of failure logs at most.
	eventsLogEvery = time.Minute
)

// The events' statements, on the primary's pool. now() is the database's clock.
// A duration of $1 is in microseconds.
const (
	// newestEvent reads the sequence of the newest row, 0 for none.
	newestEvent = `SELECT coalesce(max(seq), 0) FROM podium_events`
	// insertEvent writes an event's row: the identity gives its sequence, the
	// database's clock its created_at.
	insertEvent = `INSERT INTO podium_events (kind, payload) VALUES ($1, $2)`
	// readEvents reads up to $2 rows after the sequence $1, in sequence.
	readEvents = `SELECT seq, kind, payload FROM podium_events WHERE seq > $1 ORDER BY seq LIMIT $2`
	// pruneEvents deletes the rows created longer ago than $1.
	pruneEvents = `DELETE FROM podium_events WHERE created_at < now() - $1::bigint * interval '1 microsecond'`
)

// errTransportRunTwice is the error of a second Run of a transport.
var errTransportRunTwice = errors.New("database: the events transport ran already: a transport runs once")

// EventTransport is the event.Writer of the database mode: it shares the event
// hub's events between the replicas through the table podium_events, so that
// the UI on every replica sees what any replica sent.
//
// Notify and Record queue the event, which a writer of the transport's own
// writes as a row, and every replica applies through ApplyLocalNotify and
// ApplyLocalRecord once it reads the row back: this one too, never at the call,
// so that each replica applies each event once, in the rows' order. DeleteIssues
// removes the matching issues on this replica at the call and returns how many,
// as the handlers decide on that count, and queues the deletion as a row too,
// which every replica applies when it reads it, this one again, in sequence:
// there it removes the issues whose rows came before the deletion's, though it
// read them after the call, as every other replica does.
//
// The queue holds queueSize events. Queueing never waits: an event that finds
// the queue full, or the transport stopped, or that does not encode, never
// reaches the database, and is applied on this replica alone, as raft's
// EventWriter falls back, and counted (Dropped). The writer makes one attempt
// per batch of queued events, bounded by the write timeout: one transaction
// takes the dialect's event lock, so that the rows' sequence order is their
// commit order, inserts the rows and sends the dialect's notification, which
// goes out with the commit. A write that fails before COMMIT applies its events
// on this replica alone and counts them; one whose COMMIT fails may have
// written them, and leaves them to the reads.
//
// Each replica reads the rows after the last sequence it read, in sequence:
// when its waker wakes it, after each write of its own, and on an interval (a
// few seconds with a waker, a second without one). It starts after the newest
// row present when Run starts: the hub's history stays transient, and the events
// queued before then are written once Run knows where it starts. A row of an
// unknown kind, or whose payload does not decode, is skipped and logged without
// its payload, and the reads go on past it, so that a newer release never stalls
// an older one during a rolling upgrade. The leader prunes the rows older than
// ten minutes by the database's clock, every minute.
//
// No log line carries a payload, a statement's argument or a connection
// string; failures log at most once a minute per kind, with the number of lines
// held back since the last.
type EventTransport struct {
	db     *sql.DB
	d      sqlstore.Dialect
	waker  Waker // nil: the transport polls
	hub    *event.Hub
	lead   cluster.Leadership
	logger zerolog.Logger

	// notify and lock are the dialect's statements: the wake-up, "" for none,
	// and the event lock.
	notify, lock string

	// timeout is eventsTimeout, readEvery listenedReadEvery or polledReadEvery,
	// pruneEvery eventsPruneEvery, pruneAge eventsPruneAge, logEvery
	// eventsLogEvery.
	timeout, readEvery, pruneEvery, pruneAge, logEvery time.Duration

	// queue holds the events the writer writes; written wakes the reader once a
	// write of this transport's committed.
	queue   chan entry
	written chan struct{}
	dropped atomic.Uint64
	running atomic.Bool

	// mu is read-locked to queue an event, and locked to stop the queue: no
	// event is queued once the transport stopped.
	mu      sync.RWMutex
	stopped bool

	// One limit per kind of failure the transport logs.
	dropLog, writeLog, readLog, listenLog, skipLog, pruneLog logLimit
}

// entry is a queued event: the kind and the payload of its row, and its op, to
// apply on this replica if the row never reaches the database.
type entry struct {
	kind    string
	payload []byte
	op      *raftv1.Op
	// applied says that the call applied the op already, as DeleteIssues does.
	applied bool
}

// row is an event row as the reader reads it.
type row struct {
	seq     int64
	kind    string
	payload []byte
}

// NewEventTransport returns the transport of hub's events on db, the primary's
// pool, which runs once Run is called: d gives it the event lock and the
// wake-up statement, which goes out on EventsChannel; w, a waker on that
// channel (NewPostgresWaker(url, EventsChannel)), wakes it when rows may have
// come; and lead says when it prunes. A nil w, or a dialect without
// notifications, has it poll every second; with a w it also reads every few
// seconds. Attach it to the hub with hub.SetWriter before the first event.
func NewEventTransport(db *sql.DB, d sqlstore.Dialect, w Waker, hub *event.Hub, lead cluster.Leadership, logger zerolog.Logger) *EventTransport {
	notify := d.NotifySQL(EventsChannel)
	if notify == "" {
		// Nothing would notify it: its reconnects alone would wake it.
		w = nil
	}
	readEvery := polledReadEvery
	if w != nil {
		readEvery = listenedReadEvery
	}
	return &EventTransport{
		db:         db,
		d:          d,
		waker:      w,
		hub:        hub,
		lead:       lead,
		logger:     logger,
		notify:     notify,
		lock:       d.EventLockSQL(),
		timeout:    eventsTimeout,
		readEvery:  readEvery,
		pruneEvery: eventsPruneEvery,
		pruneAge:   eventsPruneAge,
		logEvery:   eventsLogEvery,
		queue:      make(chan entry, queueSize),
		written:    make(chan struct{}, 1),
	}
}

// Notify implements event.Writer: it queues the pulse, which every replica
// applies once it reads its row back, this one included.
func (t *EventTransport) Notify(name string) {
	t.send(kindNotify, notifyOp(name), false)
}

// Record implements event.Writer: it queues the record, which every replica
// applies once it reads its row back, this one included.
func (t *EventTransport) Record(kind string, p event.Payload) {
	t.send(kindRecord, recordOp(kind, p), false)
}

// DeleteIssues implements event.Writer: it removes the issues of kind about the
// entity id on this replica at once and returns how many, then queues the
// deletion, which every replica applies once it reads its row back, this one
// again.
func (t *EventTransport) DeleteIssues(kind, id string) int {
	n := t.hub.ApplyLocalDeleteIssues(kind, id)
	t.send(kindDeleteIssues, deleteIssuesOp(kind, id), true)
	return n
}

// Dropped returns how many events never reached the database since the
// transport was built: each was applied on its replica alone.
func (t *EventTransport) Dropped() uint64 { return t.dropped.Load() }

// send queues the event op, of the row kind, without waiting: one that does
// not encode, or finds the queue full or the transport stopped, is dropped.
func (t *EventTransport) send(kind string, op *raftv1.Op, applied bool) {
	e := entry{kind: kind, op: op, applied: applied}
	payload, err := encode(op)
	if err != nil {
		t.drop(e, "the event does not encode")
		return
	}
	e.payload = payload
	t.mu.RLock()
	stopped, queued := t.stopped, false
	if !stopped {
		select {
		case t.queue <- e:
			queued = true
		default:
		}
	}
	t.mu.RUnlock()
	switch {
	case stopped:
		t.drop(e, "")
	case !queued:
		t.drop(e, "the event queue is full")
	}
}

// drop applies e on this replica alone, unless its call did, and counts it: it
// never reaches the database. A reason logs, rate-limited; none, for an event
// that comes once the transport stopped, is quiet.
func (t *EventTransport) drop(e entry, reason string) {
	total := t.dropped.Add(1)
	if !e.applied {
		t.apply(e.op)
	}
	if reason == "" {
		return
	}
	if ok, held := t.dropLog.allow(t.logEvery); ok {
		t.logger.Warn().Str("reason", reason).Uint64("dropped", total).Int("held_back", held).
			Msg("database: an event never reached the database: it was applied on this replica alone")
	}
}

// Run shares the events until ctx ends: it reads where the rows end, retrying
// until it can, then writes the queued events, reads the rows the other
// replicas write, listens for their wake-ups, and prunes the rows while lead
// says this replica leads. When ctx ends it stops: every event still queued,
// and every event sent after, is applied on this replica alone and counted
// dropped. It returns nil, or at once an error for a second Run.
func (t *EventTransport) Run(ctx context.Context) error {
	if !t.running.CompareAndSwap(false, true) {
		return errTransportRunTwice
	}
	defer t.stop()
	last, ok := t.start(ctx)
	if !ok {
		return nil
	}
	wake := make(chan struct{}, 1)
	var wg sync.WaitGroup
	if t.waker != nil {
		wg.Go(func() { t.listen(ctx, wake) })
	}
	wg.Go(func() { t.write(ctx) })
	wg.Go(func() { t.read(ctx, last, wake) })
	wg.Go(func() { _ = t.lead.WhenLeader(ctx, t.prune) })
	wg.Wait()
	return nil
}

// stop ends the transport's queue: no event is queued once it holds mu, and
// those queued are applied on this replica alone and counted dropped.
func (t *EventTransport) stop() {
	t.mu.Lock()
	t.stopped = true
	t.mu.Unlock()
	for {
		select {
		case e := <-t.queue:
			t.drop(e, "")
		default:
			return
		}
	}
}

// start returns the sequence of the newest row, where the reads start, and
// whether ctx was live once it read it: it retries every read interval until
// it can.
func (t *EventTransport) start(ctx context.Context) (int64, bool) {
	for {
		last, err := t.newest(ctx)
		if err == nil {
			return last, true
		}
		if ctx.Err() != nil {
			return 0, false
		}
		t.warn(&t.readLog, err, "database: reading where the events end failed: retrying; the events wait in the queue")
		timer := time.NewTimer(t.readEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, false
		case <-timer.C:
		}
	}
}

// newest reads the sequence of the newest row, 0 for none.
func (t *EventTransport) newest(ctx context.Context) (int64, error) {
	stmt, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	var last int64
	if err := t.db.QueryRowContext(stmt, newestEvent).Scan(&last); err != nil {
		return 0, t.failed(stmt, err)
	}
	return last, nil
}

// write writes the queued events until ctx ends: each time it takes the events
// queued, writeBatch at most, and writes them in one transaction.
func (t *EventTransport) write(ctx context.Context) {
	batch := make([]entry, 0, writeBatch)
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-t.queue:
			batch = append(batch[:0], e)
		}
	fill:
		for len(batch) < writeBatch {
			select {
			case e := <-t.queue:
				batch = append(batch, e)
			default:
				break fill
			}
		}
		t.writeRows(ctx, batch)
	}
}

// writeRows writes batch in one transaction, one attempt bounded by the write
// timeout: the event lock, a row per event, the wake-up, then COMMIT, after
// which the reader of this replica reads. A write that fails before COMMIT
// applies the events on this replica alone and counts them dropped; a COMMIT
// that fails leaves them to the reads, which apply them if it committed.
func (t *EventTransport) writeRows(ctx context.Context, batch []entry) {
	stmt, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	tx, err := t.db.BeginTx(stmt, nil)
	if err != nil {
		t.unwritten(ctx, batch, t.failed(stmt, err))
		return
	}
	if err := t.insert(stmt, tx, batch); err != nil {
		_ = tx.Rollback()
		t.unwritten(ctx, batch, t.failed(stmt, err))
		return
	}
	if err := tx.Commit(); err != nil && ctx.Err() == nil {
		if ok, held := t.writeLog.allow(t.logEvery); ok {
			t.logger.Warn().Err(t.failed(stmt, err)).Int("events", len(batch)).Int("held_back", held).
				Msg("database: committing events failed: whether they were written is unknown; the replicas apply them if they were")
		}
	}
	signal(t.written)
}

// insert runs the statements of a write of batch in tx, up to its COMMIT. It
// returns the error of the first that fails, or of ctx once it ended.
func (t *EventTransport) insert(ctx context.Context, tx *sql.Tx, batch []entry) error {
	if t.lock != "" {
		if _, err := tx.ExecContext(ctx, t.lock); err != nil {
			return err
		}
	}
	for _, e := range batch {
		if _, err := tx.ExecContext(ctx, insertEvent, e.kind, e.payload); err != nil {
			return err
		}
	}
	if t.notify != "" {
		if _, err := tx.ExecContext(ctx, t.notify); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// unwritten applies the events of a write that failed before COMMIT on this
// replica alone, and counts them dropped. It logs, rate-limited, unless Run's
// ctx ended: a stop cuts the write in flight.
func (t *EventTransport) unwritten(ctx context.Context, batch []entry, err error) {
	for _, e := range batch {
		t.dropped.Add(1)
		if !e.applied {
			t.apply(e.op)
		}
	}
	if ctx.Err() != nil {
		return
	}
	if ok, held := t.writeLog.allow(t.logEvery); ok {
		t.logger.Warn().Err(err).Int("events", len(batch)).Uint64("dropped", t.dropped.Load()).Int("held_back", held).
			Msg("database: writing events failed: they were applied on this replica alone")
	}
}

// read applies the rows after last, in sequence, until ctx ends: at once, then
// whenever the waker wakes it, after each write of this transport, and every
// read interval.
func (t *EventTransport) read(ctx context.Context, last int64, wake <-chan struct{}) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-wake:
		case <-t.written:
		}
		last = t.readRows(ctx, last)
		timer.Reset(t.readEvery)
	}
}

// readRows applies the rows after last, in sequence, and returns the sequence
// of the last row it read, a row it skipped included. A read that fails is
// logged, rate-limited, and tried again at the next wake-up.
func (t *EventTransport) readRows(ctx context.Context, last int64) int64 {
	for {
		rows, err := t.rowsAfter(ctx, last)
		if err != nil {
			if ctx.Err() == nil {
				t.warn(&t.readLog, err, "database: reading events failed: retrying at the next wake-up")
			}
			return last
		}
		for _, r := range rows {
			t.applyRow(r)
			last = r.seq
		}
		if len(rows) < readBatch {
			return last
		}
	}
}

// rowsAfter reads readBatch rows at most after the sequence last, in sequence.
func (t *EventTransport) rowsAfter(ctx context.Context, last int64) ([]row, error) {
	stmt, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	rows, err := t.db.QueryContext(stmt, readEvents, last, readBatch)
	if err != nil {
		return nil, t.failed(stmt, err)
	}
	defer func() { _ = rows.Close() }()
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.seq, &r.kind, &r.payload); err != nil {
			return nil, t.failed(stmt, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, t.failed(stmt, err)
	}
	return out, nil
}

// applyRow applies the event of r, or skips a row this replica cannot read: an
// unknown kind, or a payload that does not decode as an event of its kind. A
// skipped row logs, rate-limited, with its sequence and kind, never its
// payload.
func (t *EventTransport) applyRow(r row) {
	if op, ok := decode(r.kind, r.payload); ok && t.apply(op) {
		return
	}
	if ok, held := t.skipLog.allow(t.logEvery); ok {
		t.logger.Warn().Int64("seq", r.seq).Str("kind", r.kind).Int("held_back", held).
			Msg("database: skipped an event row this replica cannot read, such as a newer podium's")
	}
}

// apply applies op to the hub, and reports whether it is an event this replica
// knows.
func (t *EventTransport) apply(op *raftv1.Op) bool {
	switch v := op.GetOp().(type) {
	case *raftv1.Op_EventNotify:
		t.hub.ApplyLocalNotify(v.EventNotify)
	case *raftv1.Op_EventRecord:
		t.hub.ApplyLocalRecord(v.EventRecord.GetKind(), payloadOf(v.EventRecord.GetPayload()))
	case *raftv1.Op_EventDeleteIssues:
		t.hub.ApplyLocalDeleteIssues(v.EventDeleteIssues.GetKind(), v.EventDeleteIssues.GetId())
	default:
		return false
	}
	return true
}

// listen forwards the waker's wake-ups to wake until ctx ends. A failure of
// the waker logs, rate-limited: the waker reconnects at its next Wait, after
// its backoff, and the reads on an interval go on meanwhile.
func (t *EventTransport) listen(ctx context.Context, wake chan<- struct{}) {
	for {
		err := t.waker.Wait(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			t.warn(&t.listenLog, err, "database: the events listener failed: reconnecting; the reads every few seconds go on")
		default:
			signal(wake)
		}
	}
}

// prune deletes the rows older than the prune age, at once and every prune
// interval, until ctx ends: the WhenLeader work of the transport, whose context
// ends with the term.
func (t *EventTransport) prune(ctx context.Context) error {
	ticker := time.NewTicker(t.pruneEvery)
	defer ticker.Stop()
	for {
		t.pruneRows(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// pruneRows deletes the rows older than the prune age, by the database's clock.
func (t *EventTransport) pruneRows(ctx context.Context) {
	stmt, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	res, err := t.db.ExecContext(stmt, pruneEvents, t.pruneAge.Microseconds())
	if err != nil {
		if ctx.Err() == nil {
			t.warn(&t.pruneLog, t.failed(stmt, err), "database: pruning the events failed: retrying at the next prune")
		}
		return
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		t.logger.Debug().Int64("rows", n).Dur("older_than", t.pruneAge).Msg("database: pruned the events")
	}
}

// failed returns what an error of a statement under ctx means: the error of ctx
// once ctx is done, and the dialect's classification otherwise, which names the
// engine's error code at most.
func (t *EventTransport) failed(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return t.d.Classify(err)
}

// warn logs msg at WARN with err, unless l holds the line back.
func (t *EventTransport) warn(l *logLimit, err error, msg string) {
	if ok, held := l.allow(t.logEvery); ok {
		t.logger.Warn().Err(err).Int("held_back", held).Msg(msg)
	}
}

// signal wakes whoever waits on ch, a channel of one, unless a wake-up waits
// there already.
func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// logLimit lets one line of a kind out per interval, and counts the lines it
// holds back, which the next line reports.
type logLimit struct {
	mu   sync.Mutex
	next time.Time
	held int
}

// allow reports whether a line may go out now, interval after the last, and
// how many it held back since.
func (l *logLimit) allow(interval time.Duration) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Before(l.next) {
		l.held++
		return false, 0
	}
	held := l.held
	l.next, l.held = now.Add(interval), 0
	return true, held
}

// The ops of the events, the variants of raft's replicated op that carry them:
// a row's payload is the protojson of its op.

func notifyOp(name string) *raftv1.Op {
	return &raftv1.Op{Op: &raftv1.Op_EventNotify{EventNotify: name}}
}

func recordOp(kind string, p event.Payload) *raftv1.Op {
	return &raftv1.Op{Op: &raftv1.Op_EventRecord{EventRecord: &raftv1.EventRecordMsg{
		Kind:    kind,
		Payload: &raftv1.EventPayloadMsg{Id: p.ID, By: p.By, Name: p.Name, Detail: p.Detail},
	}}}
}

func deleteIssuesOp(kind, id string) *raftv1.Op {
	return &raftv1.Op{Op: &raftv1.Op_EventDeleteIssues{EventDeleteIssues: &raftv1.EventDeleteIssuesMsg{Kind: kind, Id: id}}}
}

// payloadOf is the event payload of p, the zero payload for none.
func payloadOf(p *raftv1.EventPayloadMsg) event.Payload {
	return event.Payload{ID: p.GetId(), By: p.GetBy(), Name: p.GetName(), Detail: p.GetDetail()}
}

// encode returns the payload of op's row: its protojson, which refuses a string
// that is not UTF-8, as raft's encoding of the op does.
func encode(op *raftv1.Op) ([]byte, error) { return protojson.Marshal(op) }

// decode returns the op of a row of kind with payload, and whether it is one:
// fields it does not know are dropped, and an op other than the kind's is none.
func decode(kind string, payload []byte) (*raftv1.Op, bool) {
	op := &raftv1.Op{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(payload, op); err != nil {
		return nil, false
	}
	switch op.GetOp().(type) {
	case *raftv1.Op_EventNotify:
		return op, kind == kindNotify
	case *raftv1.Op_EventRecord:
		return op, kind == kindRecord
	case *raftv1.Op_EventDeleteIssues:
		return op, kind == kindDeleteIssues
	}
	return nil, false
}

// Waker wakes the transport when new events may be there.
type Waker interface {
	// Wait returns nil when new events may be there: a notification came, or
	// the waker connected anew, as it may have missed some while it did not
	// listen. It returns the error of a connection that failed or was lost,
	// after which the next Wait connects again, after a backoff that grows
	// with the failures in a row; and ctx's error once ctx ends. One goroutine
	// calls it at a time.
	Wait(ctx context.Context) error
}

// The Postgres waker's timings. A pgWaker keeps each in a field, which tests
// change before its first Wait.
const (
	// listenTimeout bounds a connection of the waker and its LISTEN.
	listenTimeout = 5 * time.Second
	// reconnectMin is the pause before the first reconnect after a failure; it
	// doubles with each failure in a row, up to reconnectMax.
	reconnectMin = 100 * time.Millisecond
	// reconnectMax is the longest pause before a reconnect: the reads on an
	// interval come as often.
	reconnectMax = listenedReadEvery
)

// listenerName is the application_name of the waker's connection, which names
// it in pg_stat_activity.
const listenerName = "podium-events"

// errListenerURL is the error of a connection string that does not parse,
// which never names the string: it carries a password.
var errListenerURL = errors.New("database: the events listener's connection string does not parse")

// pgWaker is the Waker of Postgres: a LISTEN on a connection of its own, not one
// of a pool. Its Notify side is the dialect's NotifySQL, which the transport
// sends after the rows it writes.
type pgWaker struct {
	connString, channel string
	// timeout is listenTimeout; backoffMin and backoffMax are reconnectMin and
	// reconnectMax.
	timeout, backoffMin, backoffMax time.Duration

	// conn is the connection that listens, nil until Wait connects it; failures
	// counts the failed or lost connections in a row.
	conn     *pgx.Conn
	failures int
}

// NewPostgresWaker returns the waker that listens on channel through a
// connection of its own to connString, with application_name podium-events,
// which its first Wait opens. LISTEN needs a direct connection, or a pooler's
// session mode: a pooler in transaction mode gives it no notification. Its
// errors name the SQLSTATE of a server's refusal at most, never the connection
// string, its user, host or password.
func NewPostgresWaker(connString, channel string) Waker {
	return &pgWaker{
		connString: connString,
		channel:    channel,
		timeout:    listenTimeout,
		backoffMin: reconnectMin,
		backoffMax: reconnectMax,
	}
}

// Wait implements Waker: it connects and listens when it has no connection, and
// returns nil once it does, as that is a wake-up; otherwise it waits for a
// notification. A connection that fails, or is lost, is closed, and the next
// Wait connects again after the backoff; so is the connection once ctx ends.
func (w *pgWaker) Wait(ctx context.Context) error {
	if w.conn == nil {
		if err := w.pause(ctx); err != nil {
			return err
		}
		if err := w.connect(ctx); err != nil {
			w.failures++
			return err
		}
		w.failures = 0
		return nil
	}
	if _, err := w.conn.WaitForNotification(ctx); err != nil {
		w.close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		w.failures++
		return failed(ctx, err)
	}
	return nil
}

// pause waits before a reconnect, after failures in a row: backoffMin after the
// first, doubling up to backoffMax; nothing before the first connection. It
// returns ctx's error once ctx ends.
func (w *pgWaker) pause(ctx context.Context) error {
	if w.failures == 0 {
		return ctx.Err()
	}
	d := w.backoffMax
	if shift := w.failures - 1; shift < 32 {
		d = min(w.backoffMin<<shift, w.backoffMax)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// connect opens the waker's connection and listens on its channel, within the
// timeout. Its errors pass through Postgres's classification, which names the
// SQLSTATE at most.
func (w *pgWaker) connect(ctx context.Context) error {
	cfg, err := pgx.ParseConfig(w.connString)
	if err != nil {
		return errListenerURL
	}
	cfg.RuntimeParams["application_name"] = listenerName
	stmt, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(stmt, cfg)
	if err != nil {
		return failed(ctx, err)
	}
	if _, err := conn.Exec(stmt, "LISTEN "+pgx.Identifier{w.channel}.Sanitize()); err != nil {
		closeConn(conn)
		return failed(ctx, err)
	}
	w.conn = conn
	return nil
}

// close closes the waker's connection, if it has one.
func (w *pgWaker) close() {
	if w.conn != nil {
		closeConn(w.conn)
		w.conn = nil
	}
}

// closeConn closes conn, waiting a second at most for the server.
func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}
