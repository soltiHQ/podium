// Package database elects podium's leader through the database, for the
// database mode, whose replicas hold no state: a lease that one replica holds at
// a time, a fence on every write transaction of the store, and a registry of
// the replicas. Leadership is the mode's cluster.Leadership, cluster.Membership
// and sqlstore.Fence, and Run takes part in the election.
//
// # The lease
//
// podium_leader holds one row: its epoch, its holder (a replica's ID), the
// address the others forward writes to, and expires_at, in the database's
// clock. A replica takes the row when it is free or expired, in one statement
// that locks the row FOR UPDATE, which waits for the old leader's writes in
// flight, then raises the epoch, also when this replica held it last: the lease
// it takes starts once the lock is taken. While it holds the row it renews it
// every third of the lease, and it steps down, AmLeader false, the contexts of
// WhenLeader cancelled and its writes refused, once no renewal has succeeded
// within two thirds of the lease, counted from the send of the last renewal
// that did: it stops before the row can expire, whatever the renewal's answer
// took. The lease's statements run on a pool of their own (Options.LeaseDB),
// so that writers never starve them, each under a statement timeout that the
// server enforces, a renewal's cut to the time left before the step-down, and
// at READ COMMITTED, whatever the database's or the role's default, so that one
// that waited for the row's lock reads the row's newest version. When Run's
// context ends, the holder releases the row once its WhenLeader work has
// returned: expires_at becomes the database's now, epoch and holder kept. Work
// that has not returned within a sixth of the lease leaves the row to expire.
//
// # The fence
//
// Check runs at the start of each write transaction of the store. A replica that
// does not hold the lease is refused without a statement. The holder reads the
// row FOR KEY SHARE and compares its epoch and holder with its own: a row that
// moved, a takeover this replica has not seen yet, is refused too, and steps it
// down at once. Both refusals are cluster.ErrNotLeader, as nothing was written.
// The key-share lock holds the row until the transaction ends against the
// takeover's FOR UPDATE alone: a takeover waits for the writes in flight, and a
// fence that starts while a takeover holds the row waits for it, then reads the
// row it left. The renewals and the release change expires_at alone, an UPDATE
// that takes the row FOR NO KEY UPDATE, which the key-share lock does not hold:
// a write transaction of any length keeps the leadership.
//
// The invariant: a statement that changes epoch or holder must lock the row
// with SELECT … FOR UPDATE first, in the same transaction; the takeover's is
// its statement's own, whose CTE takes the lock. An UPDATE without that lock is
// a no-key update: a fence neither waits for it nor re-reads the row after it.
//
// # Followers
//
// A replica that does not hold the lease reads the row every second, and
// answers CurrentLeader from what it read: the holder's address while its lease
// lasts, "" once it ended. It takes the lease when it reads it free or expired,
// and looks again at the expiry it read, so that it takes an expired lease at
// once rather than at its next poll.
//
// # The registry
//
// Each replica heartbeats its row of podium_replicas every second: its ID, its
// advertise address and the digests of the settings every replica must share
// (cluster.SharedSettings), with last_seen in the database's clock. Members are
// the replicas seen within three heartbeats, from the list the last heartbeat
// read. When Run's context ends, a replica deletes its row; the leader prunes
// the rows unseen for an hour, of replicas that never did.
//
// # Events
//
// EventTransport is the mode's event.Writer: it shares the event hub's events
// between the replicas as rows of podium_events, which every replica applies in
// sequence, woken by a LISTEN of its own (NewPostgresWaker). Its doc gives the
// rules.
//
// The statements and the error classification are Postgres's, through
// sqlstore.Postgres: no error and no log line carries a statement's arguments,
// an engine's detail or a connection string. A second engine brings its own.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
)

var (
	_ cluster.Leadership = (*Leadership)(nil)
	_ cluster.Membership = (*Leadership)(nil)
	_ sqlstore.Fence     = (*Leadership)(nil)
)

// DefaultLease is the lease of an Options.Lease left zero.
const DefaultLease = 10 * time.Second

// minLease is the shortest lease New takes, which the configuration checks
// too: a sixth of the lease bounds a lease statement, which needs
// minStatementTimeout at least.
const minLease = cluster.MinDatabaseLease

// The intervals that do not follow the lease. A Leadership keeps each in a
// field, which tests shorten before Run.
const (
	// pollInterval is how often a follower reads the lease row.
	pollInterval = time.Second
	// heartbeatInterval is how often a replica heartbeats its registry row.
	heartbeatInterval = time.Second
	// pruneInterval is how often the leader prunes the registry.
	pruneInterval = time.Minute
	// pruneAge is how long a registry row goes unseen before the leader prunes it.
	pruneAge = time.Hour
)

// errRunTwice is the error of a second Run.
var errRunTwice = errors.New("database: the leadership ran already: a Leadership runs once")

// workNotEnded is what Run's end logs when the WhenLeader work it cancelled has
// not returned within its grace, and the lease is left to expire.
const workNotEnded = "database: the leader-only work has not ended: the lease is not released, and expires a lease after its last renewal, which only delays another replica's leader-only work: the process exit bounds the overlap within the shutdown deadline, and the database writes stay fenced while calls to agents do not"

// Options are what New builds a Leadership from.
type Options struct {
	// DB is the primary's pool: the registry's statements run on it.
	DB *sql.DB
	// LeaseDB is a pool of its own on the primary, of one connection, for the
	// lease's statements alone, so that writers never starve them.
	LeaseDB *sql.DB
	// ReplicaID names this replica in the lease and the registry: random per
	// process.
	ReplicaID string
	// AdvertiseAddr is how the other replicas reach this one: host:port, an IPv6
	// host in brackets. They forward writes to its host, at the port of the
	// listener a write came in on, as every replica serves the same ports: its
	// own port is not used.
	AdvertiseAddr string
	// Lease is how long the lease lasts after its last renewal: DefaultLease
	// when zero.
	Lease time.Duration
	// Settings are the settings every replica must share: the registry holds
	// their digests.
	Settings cluster.SharedSettings
	// Observer is told of the lease's events, for the metrics.
	Observer Observer
	// Logger gets the leadership's lines, which name the replica ID, the epoch
	// and the advertise address, never a database URL or a credential.
	Logger zerolog.Logger
}

// Observer is told of the lease's events, for the metrics; a nil function is
// told nothing. Its functions run on the goroutines of the leadership and of
// the store's writes, and must not block.
type Observer struct {
	// Renewed is called after each renewal of the lease that landed.
	Renewed func()
	// Lost is called each time a term of this replica ends before Run's end: at
	// its step-down deadline, or once a renewal or a write found the lease
	// moved to another replica. Run's end, which releases the lease, is none.
	Lost func()
}

// observe calls fn, a function of an Observer, unless it is nil.
func observe(fn func()) {
	if fn != nil {
		fn()
	}
}

// Leadership is the database mode's leadership. Run takes part in the
// election and heartbeats the registry; AmLeader, CurrentLeader and WhenLeader
// answer from what it saw, Members from the registry, and Check fences the
// store's write transactions.
type Leadership struct {
	db, leaseDB *sql.DB
	id, addr    string
	digests     []byte // the registry's: Settings.Digests, the groups in order
	observer    Observer
	logger      zerolog.Logger

	// The timings that follow the lease: a renewal every third of it
	// (renewEvery), the step-down two thirds after the send of the last renewal
	// that succeeded (stepDownAfter), a lease statement's timeout a sixth
	// (statementTimeout), the pause after a lease statement that failed a
	// twelfth (retryPause), a follower's look past the expiry it read a
	// hundredth (slack), and the wait of Run's end for the WhenLeader work it
	// cancelled a sixth (workGrace).
	lease, renewEvery, stepDownAfter, statementTimeout, retryPause, slack, workGrace time.Duration
	// poll, heartbeat, pruneEvery and pruneAfter are pollInterval,
	// heartbeatInterval, pruneInterval and pruneAge.
	poll, heartbeat, pruneEvery, pruneAfter time.Duration

	running atomic.Bool
	// wake wakes the lease loop when a term ends.
	wake chan struct{}
	// leaseFailing and registryFailing say whether the last lease statement, and
	// the last heartbeat, failed: the first failure of a run logs, and the
	// success that ends it. Each loop owns its own.
	leaseFailing, registryFailing bool

	mu sync.Mutex
	// term is the term this replica leads; nil when it leads none.
	term *term
	// held is the epoch of the last term this replica took, which Run's end
	// releases; 0 when it took none.
	held int64
	// acquired closes when the next term begins.
	acquired chan struct{}
	// stopped closes once Run stopped.
	stopped chan struct{}
	// holder is the lease's holder as this replica read it last, as a follower.
	holder seen
	// members are the registry's replicas as the last heartbeat read them.
	members []member
	// watchers are woken at each change of the leader this replica knows
	// (WatchLeader).
	watchers map[chan<- struct{}]struct{}
	// working counts the calls of fn that WhenLeader runs and that have not
	// returned; idle is closed while none runs, and replaced when one starts.
	working int
	idle    chan struct{}
}

// term is a leadership term of this replica: the lease it took at an epoch.
type term struct {
	epoch int64
	// deadline is when the replica steps down without a renewal: the send of the
	// last renewal that succeeded, or of the takeover, plus two thirds of the
	// lease.
	deadline time.Time
	// timer ends the term at its deadline.
	timer *time.Timer
	// grants are the calls of fn that WhenLeader runs in the term, each
	// cancelled when it ends.
	grants map[*grant]struct{}
}

// grant is a call of fn that WhenLeader runs in a term.
type grant struct{ cancel context.CancelFunc }

// seen is the lease's holder as a follower read it.
type seen struct {
	id, address string
	// until is when its lease ends, by this replica's clock.
	until time.Time
}

// New returns the leadership of opts, which runs once Run is called. It refuses
// a missing pool, the primary's pool as the lease's, an empty replica ID or one
// that is not UTF-8 without NUL, as the registry's text column needs, an
// advertise address that cluster.CheckAdvertiseAddr refuses, and a negative
// lease or one under minLease; a zero lease is DefaultLease. It connects to
// nothing.
func New(opts Options) (*Leadership, error) {
	switch {
	case opts.DB == nil:
		return nil, errors.New("database: the leadership needs the primary's pool, Options.DB")
	case opts.LeaseDB == nil:
		return nil, errors.New("database: the leadership needs a pool of its own for the lease, Options.LeaseDB")
	case opts.LeaseDB == opts.DB:
		return nil, errors.New("database: the lease needs a pool of its own, Options.LeaseDB, not the primary's, whose writers would starve it")
	case opts.ReplicaID == "":
		return nil, errors.New("database: the replica ID is empty")
	case !text(opts.ReplicaID):
		return nil, errors.New("database: the replica ID is not UTF-8 without NUL, which the registry's text column needs")
	case opts.Lease < 0:
		return nil, fmt.Errorf("database: the lease %s is negative", opts.Lease)
	case opts.Lease > 0 && opts.Lease < minLease:
		return nil, fmt.Errorf("database: the lease %s is under %s: a sixth of it bounds a lease statement, which needs %s at least", opts.Lease, minLease, minStatementTimeout)
	}
	if err := cluster.CheckAdvertiseAddr(opts.AdvertiseAddr); err != nil {
		return nil, fmt.Errorf("database: the advertise address: %w", err)
	}
	lease := opts.Lease
	if lease == 0 {
		lease = DefaultLease
	}
	digests := make([]byte, 0, digestsSize)
	for _, group := range opts.Settings.Digests() {
		digests = append(digests, group[:]...)
	}
	return &Leadership{
		db:               opts.DB,
		leaseDB:          opts.LeaseDB,
		id:               opts.ReplicaID,
		addr:             opts.AdvertiseAddr,
		digests:          digests,
		observer:         opts.Observer,
		logger:           opts.Logger.With().Str("replica_id", opts.ReplicaID).Str("advertise_addr", opts.AdvertiseAddr).Logger(),
		lease:            lease,
		renewEvery:       lease / 3,
		stepDownAfter:    2 * lease / 3,
		statementTimeout: lease / 6,
		retryPause:       lease / 12,
		slack:            lease / 100,
		workGrace:        lease / 6,
		poll:             pollInterval,
		heartbeat:        heartbeatInterval,
		pruneEvery:       pruneInterval,
		pruneAfter:       pruneAge,
		wake:             make(chan struct{}, 1),
		acquired:         make(chan struct{}),
		stopped:          make(chan struct{}),
		watchers:         map[chan<- struct{}]struct{}{},
		idle:             closedChan(),
	}, nil
}

// closedChan returns a closed channel.
func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// text reports whether s is UTF-8 without NUL, as a Postgres text column takes
// it.
func text(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }

// AmLeader implements cluster.Leadership: whether this replica holds the lease,
// before its step-down deadline.
func (l *Leadership) AmLeader() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leading() != nil
}

// CurrentLeader implements cluster.Leadership: this replica's advertise address
// while it leads, and otherwise the holder's as this replica read it last, while
// that lease lasts; "" when no live holder is known, this replica's own lease
// after its step-down included.
func (l *Leadership) CurrentLeader() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, address := l.leader()
	return address
}

// Leader returns the ID and the advertise address of the lease's holder as this
// replica knows it, as CurrentLeader answers: its own while it leads, the
// holder's it read last while that lease lasts, and none otherwise. The
// shared-settings check reads the holder's digests by its ID (Digests).
func (l *Leadership) Leader() (id, address string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leader()
}

// leader is Leader. l.mu must be held.
func (l *Leadership) leader() (id, address string) {
	if l.leading() != nil {
		return l.id, l.addr
	}
	h := l.holder
	if h.id == "" || h.id == l.id || !time.Now().Before(h.until) {
		return "", ""
	}
	return h.id, h.address
}

// ID returns this replica's ID, Options.ReplicaID.
func (l *Leadership) ID() string { return l.id }

// WatchLeader makes wake be woken, without blocking, at each change of the
// leader this replica knows (Leader), until the function it returns stops it:
// when it reads another holder of the lease, or none, takes the lease, or steps
// down. The shared-settings check watches it, as raft's check watches the
// leader observations of its node.
func (l *Leadership) WatchLeader(wake chan<- struct{}) (stop func()) {
	l.mu.Lock()
	l.watchers[wake] = struct{}{}
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.watchers, wake)
		l.mu.Unlock()
	}
}

// leaderChanged wakes the watchers of the leader, without blocking. l.mu must
// be held.
func (l *Leadership) leaderChanged() {
	for wake := range l.watchers {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// WhenLeader implements cluster.Leadership with raft's semantics: while this
// replica leads, it runs fn with a context cancelled when the term ends, which
// the end of the term does before anything else. If fn returns nil while the
// replica still leads, WhenLeader calls it again at once, in the same term: no
// barrier precedes a call, as the database holds every committed write, so fn
// must return only once its context ends, or it runs again and again. After a
// term ends, WhenLeader waits for the next and runs fn in it. Only ctx ending,
// Run's end, as raft's shutdown, or an error of fn, which it returns, ends
// WhenLeader; a nil fn or a done ctx runs nothing.
func (l *Leadership) WhenLeader(ctx context.Context, fn func(context.Context) error) error {
	if fn == nil || ctx.Err() != nil {
		return nil
	}
	for {
		l.mu.Lock()
		t, acquired, stopped := l.leading(), l.acquired, l.stopped
		l.mu.Unlock()
		if t == nil {
			select {
			case <-ctx.Done():
				return nil
			case <-stopped:
				return nil
			case <-acquired:
			}
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		termCtx, cancel := context.WithCancel(ctx)
		g := &grant{cancel: cancel}
		l.mu.Lock()
		joined := l.leading() == t
		if joined {
			t.grants[g] = struct{}{}
			l.workStarted()
		}
		l.mu.Unlock()
		if !joined {
			cancel()
			continue
		}
		err := fn(termCtx)
		l.mu.Lock()
		delete(t.grants, g)
		l.workEnded()
		l.mu.Unlock()
		cancel()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// workStarted counts a call of fn that WhenLeader starts. l.mu must be held.
func (l *Leadership) workStarted() {
	if l.working == 0 {
		l.idle = make(chan struct{})
	}
	l.working++
}

// workEnded counts a call of fn that returned, and closes idle once none runs.
// l.mu must be held.
func (l *Leadership) workEnded() {
	l.working--
	if l.working == 0 {
		close(l.idle)
	}
}

// Run takes part in the election until ctx ends: it campaigns for the lease
// while this replica does not hold it, renews it while it does, reads the
// holder for CurrentLeader, and heartbeats the registry. When ctx ends it steps
// down, cancelling the contexts of WhenLeader, releases the lease it held once
// that work has returned, and deletes its registry row, then returns nil (see
// leave). A Leadership runs once: a second Run returns an error at once.
func (l *Leadership) Run(ctx context.Context) error {
	if !l.running.CompareAndSwap(false, true) {
		return errRunTwice
	}
	l.logger.Info().Dur("lease", l.lease).Msg("database: the leadership runs: campaigning for the lease")
	var wg sync.WaitGroup
	wg.Go(func() { l.runLease(ctx) })
	wg.Go(func() { l.runRegistry(ctx) })
	wg.Wait()
	l.leave()
	return nil
}

// leave ends Run: it ends the term this replica leads, which cancels the
// contexts of WhenLeader, stops WhenLeader, waits a sixth of the lease at most
// for the calls of fn in flight to return, then releases the last lease this
// replica took, if the row still holds it, and deletes its registry row. Work
// that has not returned by then, as runners that outlived the app's drain
// deadline, may still run: the lease is not released, and expires a lease
// after its last renewal, which only delays another replica's leader-only work
// (workNotEnded, at WARN). A release or a delete that fails is
// logged: the lease then expires, and the leader prunes the row.
func (l *Leadership) leave() {
	l.mu.Lock()
	if l.term != nil {
		l.end(l.term)
	}
	epoch, idle := l.held, l.idle
	close(l.stopped)
	l.mu.Unlock()
	if epoch != 0 {
		if l.workReturns(idle) {
			l.release(epoch)
		} else {
			l.logger.Warn().Int64("epoch", epoch).Dur("lease", l.lease).Msg(workNotEnded)
		}
	}
	l.deregister()
	l.logger.Info().Msg("database: the leadership stopped")
}

// workReturns waits for idle, closed once no call of fn runs, workGrace at
// most, and reports whether it closed.
func (l *Leadership) workReturns(idle <-chan struct{}) bool {
	timer := time.NewTimer(l.workGrace)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

// leading returns the term this replica leads, nil when it leads none or its
// step-down deadline passed. l.mu must be held.
func (l *Leadership) leading() *term {
	if t := l.term; t != nil && time.Now().Before(t.deadline) {
		return t
	}
	return nil
}

// current returns the term this replica leads, as leading does.
func (l *Leadership) current() *term {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leading()
}

// begin starts the term of the lease taken at epoch by a statement sent at
// sent: it leads until two thirds of the lease after sent, unless a renewal
// moves that deadline, and WhenLeader runs fn in it.
func (l *Leadership) begin(epoch int64, sent time.Time) {
	t := &term{epoch: epoch, deadline: sent.Add(l.stepDownAfter), grants: map[*grant]struct{}{}}
	l.mu.Lock()
	t.timer = time.AfterFunc(time.Until(t.deadline), func() { l.lapse(t) })
	l.term, l.held, l.holder = t, epoch, seen{}
	close(l.acquired)
	l.acquired = make(chan struct{})
	l.leaderChanged()
	l.mu.Unlock()
	l.logger.Info().Int64("epoch", epoch).Msg("database: took the lease: this replica leads")
}

// renewed moves the deadline of t to two thirds of the lease after sent, the
// send of a renewal that succeeded, unless t ended or its deadline passed: a
// renewal that answers after the step-down revives nothing.
func (l *Leadership) renewed(t *term, sent time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.term != t || !time.Now().Before(t.deadline) {
		return
	}
	t.deadline = sent.Add(l.stepDownAfter)
	t.timer.Reset(time.Until(t.deadline))
}

// lapse ends t at its deadline, when no renewal moved it.
func (l *Leadership) lapse(t *term) {
	l.mu.Lock()
	if l.term != t {
		l.mu.Unlock()
		return
	}
	if left := time.Until(t.deadline); left > 0 {
		t.timer.Reset(left)
		l.mu.Unlock()
		return
	}
	l.end(t)
	l.mu.Unlock()
	l.logger.Warn().Int64("epoch", t.epoch).Dur("lease", l.lease).
		Msg("database: no renewal of the lease succeeded within two thirds of the lease: stepping down")
	observe(l.observer.Lost)
}

// stepDown ends t, if it is the term this replica leads, and reports whether
// it did.
func (l *Leadership) stepDown(t *term) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.term != t {
		return false
	}
	l.end(t)
	return true
}

// end ends t, the term this replica leads: AmLeader turns false, the contexts
// of WhenLeader are cancelled, the lease loop wakes up to follow, and the
// watchers of the leader wake. l.mu must be held.
func (l *Leadership) end(t *term) {
	t.timer.Stop()
	l.term = nil
	for g := range t.grants {
		g.cancel()
	}
	clear(t.grants)
	select {
	case l.wake <- struct{}{}:
	default:
	}
	l.leaderChanged()
}

// failed returns what an error of a statement under ctx means: the error of ctx
// once ctx is done, and Postgres's classification otherwise, which names the
// SQLSTATE at most, never a row, an argument or a connection string.
func failed(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return sqlstore.Postgres{}.Classify(err)
}
