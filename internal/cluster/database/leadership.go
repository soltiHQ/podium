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
// that raises the epoch, also when this replica held it last. While it holds the
// row it renews it every third of the lease, and it steps down, AmLeader false,
// the contexts of WhenLeader cancelled and its writes refused, once no renewal
// has succeeded within two thirds of the lease, counted from the send of the
// last renewal that did: it stops before the row can expire, whatever the
// renewal's answer took. The lease's statements run on a pool of their own
// (Options.LeaseDB), each under a statement timeout that the server enforces,
// so that writers never starve them and none runs past the step-down. When Run's
// context ends, the holder releases the row: expires_at becomes the database's
// now, epoch and holder kept.
//
// # The fence
//
// Check runs at the start of each write transaction of the store. A replica that
// does not hold the lease is refused without a statement. The holder reads the
// row FOR SHARE and compares its epoch and holder with its own: a row that moved,
// a takeover this replica has not seen yet, is refused too, and steps it down at
// once. Both refusals are cluster.ErrNotLeader, as nothing was written. The
// share lock holds the row until the transaction ends: a takeover waits for the
// write in flight, and a write after the takeover reads the row it left.
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
	"net"
	"strconv"
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

// minLease is the shortest lease New takes: a sixth of the lease bounds a lease
// statement, which needs minStatementTimeout at least.
const minLease = 6 * minStatementTimeout

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
	// Logger gets the leadership's lines, which name the replica ID, the epoch
	// and the advertise address, never a database URL or a credential.
	Logger zerolog.Logger
}

// Leadership is the database mode's leadership. Run takes part in the
// election and heartbeats the registry; AmLeader, CurrentLeader and WhenLeader
// answer from what it saw, Members from the registry, and Check fences the
// store's write transactions.
type Leadership struct {
	db, leaseDB *sql.DB
	id, addr    string
	digests     []byte // the registry's: Settings.Digests, the groups in order
	logger      zerolog.Logger

	// The timings that follow the lease: a renewal every third of it
	// (renewEvery), the step-down two thirds after the send of the last renewal
	// that succeeded (stepDownAfter), a lease statement's timeout a sixth
	// (statementTimeout), the pause after a lease statement that failed a
	// twelfth (retryPause), and a follower's look past the expiry it read a
	// hundredth (slack).
	lease, renewEvery, stepDownAfter, statementTimeout, retryPause, slack time.Duration
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
// a missing pool, an empty replica ID or one that is not UTF-8 without NUL, as
// the registry's text column needs, an advertise address that is not host:port
// with a port number and a host to dial, and a negative lease or one under
// minLease; a zero lease is DefaultLease. It connects to nothing.
func New(opts Options) (*Leadership, error) {
	switch {
	case opts.DB == nil:
		return nil, errors.New("database: the leadership needs the primary's pool, Options.DB")
	case opts.LeaseDB == nil:
		return nil, errors.New("database: the leadership needs a pool of its own for the lease, Options.LeaseDB")
	case opts.ReplicaID == "":
		return nil, errors.New("database: the replica ID is empty")
	case !text(opts.ReplicaID):
		return nil, errors.New("database: the replica ID is not UTF-8 without NUL, which the registry's text column needs")
	case opts.Lease < 0:
		return nil, fmt.Errorf("database: the lease %s is negative", opts.Lease)
	case opts.Lease > 0 && opts.Lease < minLease:
		return nil, fmt.Errorf("database: the lease %s is under %s: a sixth of it bounds a lease statement, which needs %s at least", opts.Lease, minLease, minStatementTimeout)
	}
	if err := checkAdvertise(opts.AdvertiseAddr); err != nil {
		return nil, fmt.Errorf("database: advertise address %q: %w", opts.AdvertiseAddr, err)
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
		logger:           opts.Logger.With().Str("replica_id", opts.ReplicaID).Str("advertise_addr", opts.AdvertiseAddr).Logger(),
		lease:            lease,
		renewEvery:       lease / 3,
		stepDownAfter:    2 * lease / 3,
		statementTimeout: lease / 6,
		retryPause:       lease / 12,
		slack:            lease / 100,
		poll:             pollInterval,
		heartbeat:        heartbeatInterval,
		pruneEvery:       pruneInterval,
		pruneAfter:       pruneAge,
		wake:             make(chan struct{}, 1),
		acquired:         make(chan struct{}),
		stopped:          make(chan struct{}),
	}, nil
}

// checkAdvertise says what is wrong with addr as an advertise address, if
// anything: it must be host:port, an IPv6 host in brackets, with a port number
// from 0 to 65535 and a host the others can dial, not none or every interface,
// in text a text column holds.
func checkAdvertise(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	switch {
	case err != nil:
		return fmt.Errorf("not host:port: %w", err)
	case !text(addr):
		return errors.New("not UTF-8 without NUL, which the registry's text column needs")
	case host == "":
		return errors.New("no host for the other replicas to dial")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("the port %q is not a number from 0 to 65535", port)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return errors.New("every interface, which no other replica can dial")
	}
	return nil
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
	if l.leading() != nil {
		return l.addr
	}
	h := l.holder
	if h.id == "" || h.id == l.id || !time.Now().Before(h.until) {
		return ""
	}
	return h.address
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
		}
		l.mu.Unlock()
		if !joined {
			cancel()
			continue
		}
		err := fn(termCtx)
		l.mu.Lock()
		delete(t.grants, g)
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

// Run takes part in the election until ctx ends: it campaigns for the lease
// while this replica does not hold it, renews it while it does, reads the
// holder for CurrentLeader, and heartbeats the registry. When ctx ends it steps
// down, cancelling the contexts of WhenLeader, releases the lease it held, and
// deletes its registry row, then returns nil. A Leadership runs once: a second
// Run returns an error at once.
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
// contexts of WhenLeader, stops WhenLeader, then releases the last lease this
// replica took, if the row still holds it, and deletes its registry row. A
// release or a delete that fails is logged: the lease then expires, and the
// leader prunes the row.
func (l *Leadership) leave() {
	l.mu.Lock()
	if l.term != nil {
		l.end(l.term)
	}
	epoch := l.held
	close(l.stopped)
	l.mu.Unlock()
	if epoch != 0 {
		l.release(epoch)
	}
	l.deregister()
	l.logger.Info().Msg("database: the leadership stopped")
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
// of WhenLeader are cancelled, and the lease loop wakes up to follow. l.mu must
// be held.
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
