package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// The tests of the database leadership run on Postgres, each on a database of
// its own from pgtest, and skip without one, but for TestNewValidatesOptions.
// Their leases are seconds long, and they wait on conditions, never on fixed
// sleeps.

// testLease is the lease of a test replica that names none: short, so that a
// failover takes seconds.
const testLease = 3 * time.Second

// waitStep is the pause between two looks of waitFor at its condition.
const waitStep = 5 * time.Millisecond

// migratedDatabase returns the URL of a fresh database with podium's schema,
// and a pool on it for the test's own statements.
func migratedDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	databaseURL := pgtest.NewDatabase(t)
	admin := pgtest.Open(t, databaseURL)
	if err := sqlstore.Migrate(t.Context(), admin, sqlstore.Postgres{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return databaseURL, admin
}

// replicaConfig is what a test replica differs in; a zero field keeps the
// default, and testLease for the lease.
type replicaConfig struct {
	lease      time.Duration
	poll       time.Duration
	heartbeat  time.Duration
	pruneEvery time.Duration
	settings   cluster.SharedSettings
	observer   Observer
}

// replica is a Leadership under test, on pools of its own.
type replica struct {
	*Leadership
	db      *sql.DB // its primary's pool
	leaseDB *sql.DB // its lease's pool, of one connection
	logs    *logBuffer
	stop    func() error // ends Run and returns its error; start sets it
}

// newReplica builds the replica id, which advertises id.podium.test:7000, on the
// database at databaseURL, without running it.
func newReplica(t *testing.T, databaseURL, id string, cfg replicaConfig) *replica {
	t.Helper()
	db := pgtest.Open(t, databaseURL)
	leaseDB := pgtest.Open(t, databaseURL)
	leaseDB.SetMaxOpenConns(1)
	if cfg.lease == 0 {
		cfg.lease = testLease
	}
	logs := &logBuffer{}
	l, err := New(Options{
		DB:            db,
		LeaseDB:       leaseDB,
		ReplicaID:     id,
		AdvertiseAddr: id + ".podium.test:7000",
		Lease:         cfg.lease,
		Settings:      cfg.settings,
		Observer:      cfg.observer,
		Logger:        zerolog.New(logs),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cfg.poll > 0 {
		l.poll = cfg.poll
	}
	if cfg.heartbeat > 0 {
		l.heartbeat = cfg.heartbeat
	}
	if cfg.pruneEvery > 0 {
		l.pruneEvery = cfg.pruneEvery
	}
	return &replica{Leadership: l, db: db, leaseDB: leaseDB, logs: logs}
}

// start runs the replica until its stop, or the test's end.
func (r *replica) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	var (
		once sync.Once
		err  error
	)
	r.stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				err = errors.New("Run did not return within 10 s of its context's end")
			}
		})
		return err
	}
	t.Cleanup(func() {
		if err := r.stop(); err != nil {
			t.Errorf("replica %s: %v", r.id, err)
		}
	})
}

// logBuffer collects a replica's log lines, which zerolog writes from several
// goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// checkLogs checks that r's log lines name the replica and an epoch, and carry
// neither the database URL nor its host or password. It never prints them.
func checkLogs(t *testing.T, r *replica) {
	t.Helper()
	logs := r.logs.String()
	if !strings.Contains(logs, `"replica_id":"`+r.id+`"`) || !strings.Contains(logs, `"epoch":`) {
		t.Errorf("the log lines of %s name no replica ID or no epoch:\n%s", r.id, logs)
	}
	server, err := url.Parse(os.Getenv(pgtest.EnvURL))
	if err != nil {
		t.Fatalf("%s does not parse", pgtest.EnvURL)
	}
	secrets := map[string]string{"a postgres:// URL": "postgres://", "the server's host": server.Host}
	if password, ok := server.User.Password(); ok {
		secrets["the password"] = password
	}
	for what, secret := range secrets {
		if secret != "" && strings.Contains(logs, secret) {
			t.Errorf("the log lines of %s carry %s", r.id, what)
		}
	}
}

// waitFor fails the test unless cond holds within the time given.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(waitStep)
	}
}

// receive returns what ch sends within the time given.
func receive[T any](t *testing.T, ch <-chan T, within time.Duration, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(within):
		t.Fatalf("%s: not within %s", what, within)
		var zero T
		return zero
	}
}

// cancelled fails the test unless ctx ends within the time given.
func cancelled(t *testing.T, ctx context.Context, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(within):
		t.Fatalf("%s: not cancelled within %s", what, within)
	}
}

// TestNewValidatesOptions checks what New refuses: a missing pool, the
// primary's pool as the lease's, whose writers would starve the lease, an empty
// replica ID or one the registry's text column cannot hold, an advertise
// address that is not host:port with a port number and a host to dial, and a
// negative lease or one too short to bound its statements; a zero lease is
// DefaultLease, and the lease sets the other timings. It needs no Postgres: New
// connects to nothing.
func TestNewValidatesOptions(t *testing.T) {
	pools := make([]*sql.DB, 2)
	for i := range pools {
		pool, err := sql.Open("pgx", "postgres://podium.invalid/podium")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pool.Close() })
		pools[i] = pool
	}
	valid := Options{DB: pools[0], LeaseDB: pools[1], ReplicaID: "replica-1", AdvertiseAddr: "10.0.0.5:0"}

	for _, tt := range []struct {
		name   string
		change func(*Options)
		ok     bool
	}{
		{"valid, the port unused", func(*Options) {}, true},
		{"an IPv6 host in brackets", func(o *Options) { o.AdvertiseAddr = "[fd00::5]:7000" }, true},
		{"a host name", func(o *Options) { o.AdvertiseAddr = "podium-0.podium.svc:7000" }, true},
		{"a lease of its own", func(o *Options) { o.Lease = time.Second }, true},
		{"no primary pool", func(o *Options) { o.DB = nil }, false},
		{"no lease pool", func(o *Options) { o.LeaseDB = nil }, false},
		{"the primary's pool as the lease's", func(o *Options) { o.LeaseDB = o.DB }, false},
		{"an empty replica ID", func(o *Options) { o.ReplicaID = "" }, false},
		{"a replica ID with a NUL", func(o *Options) { o.ReplicaID = "replica\x001" }, false},
		{"a replica ID that is not UTF-8", func(o *Options) { o.ReplicaID = "replica\xff" }, false},
		{"no advertise address", func(o *Options) { o.AdvertiseAddr = "" }, false},
		{"no port", func(o *Options) { o.AdvertiseAddr = "10.0.0.5" }, false},
		{"an empty port", func(o *Options) { o.AdvertiseAddr = "10.0.0.5:" }, false},
		{"no host", func(o *Options) { o.AdvertiseAddr = ":7000" }, false},
		{"an IPv6 host without brackets", func(o *Options) { o.AdvertiseAddr = "fd00::5:7000" }, false},
		{"a port that is no number", func(o *Options) { o.AdvertiseAddr = "10.0.0.5:http" }, false},
		{"a port over 65535", func(o *Options) { o.AdvertiseAddr = "10.0.0.5:65536" }, false},
		{"every interface", func(o *Options) { o.AdvertiseAddr = "0.0.0.0:7000" }, false},
		{"every IPv6 interface", func(o *Options) { o.AdvertiseAddr = "[::]:7000" }, false},
		{"a host with a NUL", func(o *Options) { o.AdvertiseAddr = "podium\x00:7000" }, false},
		{"a negative lease", func(o *Options) { o.Lease = -time.Second }, false},
		{"a lease too short to bound its statements", func(o *Options) { o.Lease = 59 * time.Millisecond }, false},
		{"the shortest lease", func(o *Options) { o.Lease = 60 * time.Millisecond }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := valid
			tt.change(&opts)
			l, err := New(opts)
			if tt.ok != (err == nil) || (err == nil) != (l != nil) {
				t.Fatalf("New: a leadership %v, error %v; want ok %v", l != nil, err, tt.ok)
			}
		})
	}

	for _, tt := range []struct{ lease, want time.Duration }{
		{0, DefaultLease},
		{3 * time.Second, 3 * time.Second},
	} {
		opts := valid
		opts.Lease = tt.lease
		l, err := New(opts)
		if err != nil {
			t.Fatalf("New with a lease of %s: %v", tt.lease, err)
		}
		want := tt.want
		got := []time.Duration{l.lease, l.renewEvery, l.stepDownAfter, l.statementTimeout, l.workGrace}
		if got[0] != want || got[1] != want/3 || got[2] != 2*want/3 || got[3] != want/6 || got[4] != want/6 {
			t.Errorf("a lease of %s: lease, renewal, step-down, statement timeout, work grace %v; want %s and its third, two thirds, sixth and sixth", tt.lease, got, want)
		}
		if l.poll != time.Second || l.heartbeat != time.Second || l.pruneEvery != time.Minute || l.pruneAfter != time.Hour {
			t.Errorf("poll, heartbeat, prune interval and age %s, %s, %s, %s; want 1s, 1s, 1m and 1h", l.poll, l.heartbeat, l.pruneEvery, l.pruneAfter)
		}
	}
}

// TestWhenLeaderRaftSemantics checks that WhenLeader keeps raft's semantics: fn
// runs once the replica leads, with a context cancelled when the term ends, and
// again at each acquisition; fn that returns nil while the replica still leads
// runs again at once; fn's error ends WhenLeader with it; the caller's context
// ending, and Run's end, end it with nil; a nil fn and a done context run
// nothing.
func TestWhenLeaderRaftSemantics(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// A call made before the replica leads runs fn once it does.
	terms := make(chan context.Context, 4)
	done := make(chan error, 1)
	go func() {
		done <- a.WhenLeader(ctx, func(term context.Context) error {
			terms <- term
			<-term.Done()
			return nil
		})
	}()
	a.start(t)
	first := receive(t, terms, 5*time.Second, "fn at the first term")
	if first.Err() != nil {
		t.Fatal("fn's context was cancelled at the start of its term")
	}
	epoch := readLease(t, admin).epoch

	// fn that returns nil while the replica still leads runs again, in the same
	// term.
	var calls atomic.Int32
	again := make(chan struct{})
	go func() {
		_ = a.WhenLeader(ctx, func(term context.Context) error {
			switch calls.Add(1) {
			case 1:
				return nil
			case 2:
				close(again)
			}
			<-term.Done()
			return nil
		})
	}()
	receive(t, again, 5*time.Second, "fn again after it returned in its term")
	if row := readLease(t, admin); row.epoch != epoch || !a.AmLeader() {
		t.Fatalf("fn ran again at epoch %d, replica leading %v; want the same term, %d", row.epoch, a.AmLeader(), epoch)
	}

	// fn's error ends WhenLeader with it.
	errFn := errors.New("database test: fn fails")
	if err := a.WhenLeader(ctx, func(context.Context) error { return errFn }); !errors.Is(err, errFn) {
		t.Fatalf("WhenLeader with fn failing: %v, want fn's error", err)
	}

	// The loss of the lease cancels fn's context, and the next acquisition runs
	// fn again, with a context of its own.
	moveLease(t, admin)
	cancelled(t, first, testLease, "fn's context once the lease moved")
	second := receive(t, terms, 2*testLease, "fn at the next term")
	if second == first || second.Err() != nil {
		t.Fatal("fn's next term reused the context of the first, or a cancelled one")
	}
	if row := readLease(t, admin); row.holder != a.id || row.epoch <= epoch+1 {
		t.Fatalf("the lease is %s's at epoch %d; want %s's again above the moved epoch %d", row.holder, row.epoch, a.id, epoch+1)
	}

	// The caller's context ending ends WhenLeader with nil, and fn's context.
	cancel()
	cancelled(t, second, time.Second, "fn's context once the caller's ended")
	if err := receive(t, done, time.Second, "WhenLeader's return once its context ended"); err != nil {
		t.Fatalf("WhenLeader returned %v once its context ended, want nil", err)
	}

	// A done context and a nil fn run nothing.
	if err := a.WhenLeader(ctx, func(context.Context) error { t.Error("fn ran on a done context"); return nil }); err != nil {
		t.Errorf("WhenLeader on a done context: %v, want nil", err)
	}
	if err := a.WhenLeader(t.Context(), nil); err != nil {
		t.Errorf("WhenLeader of a nil fn: %v, want nil", err)
	}

	// Run's end ends WhenLeader with nil, as raft's shutdown does.
	running := make(chan context.Context, 1)
	ended := make(chan error, 1)
	go func() {
		ended <- a.WhenLeader(context.Background(), func(term context.Context) error {
			running <- term
			<-term.Done()
			return nil
		})
	}()
	third := receive(t, running, 5*time.Second, "fn of a call made while the replica leads")
	if err := a.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if third.Err() == nil {
		t.Error("fn's context is live once Run returned")
	}
	if err := receive(t, ended, time.Second, "WhenLeader's return once Run returned"); err != nil {
		t.Errorf("WhenLeader returned %v once Run returned, want nil", err)
	}
	if err := a.WhenLeader(context.Background(), func(context.Context) error { t.Error("fn ran once Run returned"); return nil }); err != nil {
		t.Errorf("WhenLeader once Run returned: %v, want nil", err)
	}
}

// TestLeaderWatched checks that Leader names the lease's holder as each replica
// knows it, its ID and advertise address, and that WatchLeader wakes a watcher
// at each change of it: a follower that first reads a holder, and once the
// holder stopped, takes the lease itself. The function WatchLeader returns
// stops the wake-ups.
func TestLeaderWatched(t *testing.T) {
	databaseURL, _ := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{poll: 20 * time.Millisecond})
	b := newReplica(t, databaseURL, "replica-b", replicaConfig{poll: 20 * time.Millisecond})
	wake := make(chan struct{}, 1)
	stop := b.WatchLeader(wake)
	if id, addr := b.Leader(); id != "" || addr != "" {
		t.Fatalf("Leader before Run: %q at %q, want none", id, addr)
	}
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)
	if id, addr := a.Leader(); id != a.id || addr != a.addr {
		t.Errorf("the leader's Leader: %q at %q, want itself, %q at %q", id, addr, a.id, a.addr)
	}

	b.start(t)
	receive(t, wake, 5*time.Second, "a wake-up once replica-b reads replica-a's lease")
	waitFor(t, 5*time.Second, "replica-b knowing replica-a", func() bool {
		id, addr := b.Leader()
		return id == a.id && addr == a.addr
	})

	if err := a.stop(); err != nil {
		t.Fatalf("replica-a's Run: %v", err)
	}
	receive(t, wake, 5*time.Second, "a wake-up once replica-a's lease ends")
	waitFor(t, 5*time.Second, "replica-b leading", b.AmLeader)
	if id, addr := b.Leader(); id != b.id || addr != b.addr {
		t.Errorf("replica-b's Leader once it leads: %q at %q, want itself", id, addr)
	}

	stop()
	b.mu.Lock()
	watchers := len(b.watchers)
	b.mu.Unlock()
	if watchers != 0 {
		t.Errorf("%d watchers after the stop, want none", watchers)
	}
}
