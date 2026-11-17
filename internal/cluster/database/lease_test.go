package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// leaseRow is the lease row as a test reads it.
type leaseRow struct {
	epoch     int64
	holder    string
	expiresAt int64         // Unix microseconds, by the database's clock
	left      time.Duration // until expires_at, by the database's clock; negative once expired
}

// readLease reads the lease row; there must be one.
func readLease(t *testing.T, admin *sql.DB) leaseRow {
	t.Helper()
	var (
		row  leaseRow
		left int64
	)
	err := admin.QueryRowContext(t.Context(), `SELECT epoch, holder,
       floor(extract(epoch FROM expires_at) * 1000000)::bigint,
       floor(extract(epoch FROM expires_at - now()) * 1000000)::bigint
  FROM podium_leader WHERE id = 1`).Scan(&row.epoch, &row.holder, &row.expiresAt, &left)
	if err != nil {
		t.Fatalf("read the lease row: %v", err)
	}
	row.left = time.Duration(left) * time.Microsecond
	return row
}

// lockLease locks the lease row FOR UPDATE until the function it returns, or
// the test's end: every renewal and takeover waits for the lock, to its
// statement timeout, and fails, while plain reads go on.
func lockLease(t *testing.T, admin *sql.DB) (release func()) {
	t.Helper()
	tx, err := admin.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin the lock: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)
	if _, err := tx.ExecContext(context.Background(), `SELECT 1 FROM podium_leader WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatalf("lock the lease row: %v", err)
	}
	return release
}

// expireLease ends the lease in the database's view, holder and epoch kept, as a
// leader that paused for longer than its lease would find it.
func expireLease(t *testing.T, admin *sql.DB) {
	t.Helper()
	if _, err := admin.ExecContext(t.Context(), `UPDATE podium_leader SET expires_at = now() WHERE id = 1`); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
}

// expireLeaseUnlessLocked is expireLease with its UPDATE's wait for a lock
// bounded to 100 ms: past that (SQLSTATE 55P03) it leaves the lease as it is.
// An UPDATE that must wait for another update of the row in flight, as for a
// renewal not yet committed, queues behind a takeover that waits for the row,
// and would wait as long as that takeover.
func expireLeaseUnlessLocked(t *testing.T, admin *sql.DB) {
	t.Helper()
	tx, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin the expiry: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), `SET LOCAL lock_timeout = '100ms'`); err != nil {
		t.Fatalf("bound the expiry's lock wait: %v", err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE podium_leader SET expires_at = now() WHERE id = 1`); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return
		}
		t.Fatalf("expire the lease: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the expiry: %v", err)
	}
}

// moveLease hands the lease to another replica at the next epoch, expired at
// once, so that it is free to take again. It changes epoch and holder without
// locking the row FOR UPDATE first, breaking the invariant on purpose: a fence
// neither waits for it nor re-reads the row after it, which is safe only while
// no fence is in flight, so no test may use it against a write in flight.
func moveLease(t *testing.T, admin *sql.DB) {
	t.Helper()
	if _, err := admin.ExecContext(t.Context(), `UPDATE podium_leader
   SET epoch = epoch + 1, holder = 'replica-ghost', address = 'replica-ghost.podium.test:7000', expires_at = now()
 WHERE id = 1`); err != nil {
		t.Fatalf("move the lease: %v", err)
	}
}

// lockWait is how long the session on the test's database that waits for a lock
// has waited, by the database's clock: 0 while none waits.
func lockWait(t *testing.T, admin *sql.DB) time.Duration {
	t.Helper()
	var waited int64
	err := admin.QueryRowContext(t.Context(), `SELECT coalesce(max(floor(extract(epoch FROM now() - query_start) * 1000000)), 0)::bigint
  FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waited)
	if err != nil {
		t.Fatalf("read the lock wait: %v", err)
	}
	return time.Duration(waited) * time.Microsecond
}

// lockWaiters counts the sessions on the test's database that wait for a lock.
func lockWaiters(t *testing.T, admin *sql.DB) int {
	t.Helper()
	var n int
	err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity
 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
	if err != nil {
		t.Fatalf("count the lock waiters: %v", err)
	}
	return n
}

// waitBetweenRenewals waits until a quarter to seven twenty-fourths of the
// lease have passed since the leader's last renewal, by the row's expiry: no
// renewal is in flight then, and none is due before a third.
func waitBetweenRenewals(t *testing.T, admin *sql.DB, lease time.Duration) {
	t.Helper()
	waitFor(t, 3*lease, "a moment between two renewals", func() bool {
		since := lease - readLease(t, admin).left
		return since >= lease/4 && since <= 7*lease/24
	})
}

// waitForLeader waits until exactly one of a and b leads.
func waitForLeader(t *testing.T, a, b *replica) (leader, follower *replica) {
	t.Helper()
	waitFor(t, 5*time.Second, "one leader", func() bool {
		switch al, bl := a.AmLeader(), b.AmLeader(); {
		case al && !bl:
			leader, follower = a, b
		case bl && !al:
			leader, follower = b, a
		default:
			return false
		}
		return true
	})
	return leader, follower
}

// TestOneLeader checks that of two replicas on one database exactly one leads
// and runs the WhenLeader work, that both know where it serves, and that its
// term holds across renewals past the two thirds of the lease after the
// takeover that the takeover alone gives: the same leader, at the same epoch,
// its WhenLeader work run once.
func TestOneLeader(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{})
	b := newReplica(t, databaseURL, "replica-b", replicaConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runs := map[*replica]*atomic.Int32{a: {}, b: {}}
	for r, n := range runs {
		go func() {
			_ = r.WhenLeader(ctx, func(term context.Context) error {
				n.Add(1)
				<-term.Done()
				return nil
			})
		}()
	}
	a.start(t)
	b.start(t)

	leader, follower := waitForLeader(t, a, b)
	waitFor(t, 5*time.Second, "the follower knowing the leader", func() bool { return follower.CurrentLeader() == leader.addr })
	if got := leader.CurrentLeader(); got != leader.addr {
		t.Errorf("the leader's CurrentLeader is %q, want its own address %q", got, leader.addr)
	}
	first := readLease(t, admin)
	if first.holder != leader.id {
		t.Fatalf("the lease is %s's, want the leader's, %s", first.holder, leader.id)
	}

	// Two renewals land a third of the lease apart, the second two thirds of the
	// lease or more after the takeover, once the takeover's own deadline passed:
	// only the renewals keep the leader leading then.
	waitFor(t, 2*testLease, "two renewals of the lease", func() bool {
		return readLease(t, admin).expiresAt >= first.expiresAt+(2*testLease/3-testLease/12).Microseconds()
	})
	if !leader.AmLeader() || follower.AmLeader() {
		t.Fatalf("after two renewals the leader leads %v and the follower %v; want the same leader alone", leader.AmLeader(), follower.AmLeader())
	}
	if row := readLease(t, admin); row.holder != leader.id || row.epoch != first.epoch {
		t.Errorf("after two renewals the lease is %s's at epoch %d, want %s's at %d", row.holder, row.epoch, leader.id, first.epoch)
	}
	if n := runs[leader].Load(); n != 1 {
		t.Errorf("the leader ran its WhenLeader work %d times, want once: one term across the renewals", n)
	}
	if n := runs[follower].Load(); n != 0 {
		t.Errorf("the follower ran its WhenLeader work %d times, want never", n)
	}
	if got := follower.CurrentLeader(); got != leader.addr {
		t.Errorf("the follower's CurrentLeader is %q, want %q", got, leader.addr)
	}
}

// TestFailoverWithinLease checks that when the leader's lease connection closes
// between two renewals, the other replica leads within the lease, at a raised
// epoch, and the two never lead at once.
func TestFailoverWithinLease(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{})
	b := newReplica(t, databaseURL, "replica-b", replicaConfig{})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)
	b.start(t)
	waitFor(t, 5*time.Second, "replica-b following replica-a", func() bool { return b.CurrentLeader() == a.addr })
	old := readLease(t, admin)

	// Between two renewals none is in flight, so the row expires a lease after
	// the last one, which went out before the close.
	waitBetweenRenewals(t, admin, testLease)
	closed := time.Now()
	if err := a.leaseDB.Close(); err != nil {
		t.Fatalf("close replica-a's lease pool: %v", err)
	}
	for deadline := closed.Add(2 * testLease); ; {
		aLeads, bLeads := a.AmLeader(), b.AmLeader()
		if aLeads && bLeads {
			t.Fatal("both replicas lead at once")
		}
		if bLeads {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica-b did not lead within %s of the close", 2*testLease)
		}
		time.Sleep(waitStep)
	}
	took := time.Since(closed)
	t.Logf("replica-b led %s after replica-a's lease connection closed", took)
	if took > testLease {
		t.Errorf("replica-b led %s after replica-a's lease connection closed, want within the lease, %s", took, testLease)
	}
	if a.AmLeader() {
		t.Error("replica-a still leads")
	}
	if row := readLease(t, admin); row.holder != b.id || row.epoch <= old.epoch {
		t.Errorf("the lease is %s's at epoch %d, want %s's above %d", row.holder, row.epoch, b.id, old.epoch)
	}
	checkLogs(t, a)
}

// TestSameHolderNewEpoch checks that a replica that lost the lease and takes it
// again raises the epoch, as every takeover does, and takes it only once it
// expired.
func TestSameHolderNewEpoch(t *testing.T) {
	const lease = 2 * time.Second
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{lease: lease})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)

	release := lockLease(t, admin)
	// No renewal lands once the row is locked.
	old := readLease(t, admin)
	waitFor(t, 2*lease, "replica-a stepping down", func() bool { return !a.AmLeader() })
	release()
	waitFor(t, 2*lease, "replica-a leading again", a.AmLeader)

	row := readLease(t, admin)
	if row.holder != a.id || row.epoch != old.epoch+1 {
		t.Fatalf("the lease is %s's at epoch %d, want %s's at %d", row.holder, row.epoch, a.id, old.epoch+1)
	}
	// The takeover's clock_timestamp() is its expires_at less the lease.
	if took := row.expiresAt - lease.Microseconds(); took < old.expiresAt {
		t.Errorf("replica-a took the lease again %d µs before it expired, want once it expired", old.expiresAt-took)
	}
}

// slowRenewal is how long TestStepDownBeforeExpiry holds a renewal on a lock
// before it lets it land: three fifths of its statement timeout, a sixth of the
// lease.
const slowRenewal = testLease / 10

// TestStepDownBeforeExpiry checks that a leader whose renewals no longer land
// steps down, AmLeader false and its WhenLeader context cancelled, two thirds of
// the lease after the send of its last renewal that did, a slow one: before the
// row expires, with a third of the lease left, not less by the time that
// renewal took to answer.
func TestStepDownBeforeExpiry(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	terms := make(chan context.Context, 1)
	go func() {
		_ = a.WhenLeader(ctx, func(term context.Context) error {
			terms <- term
			<-term.Done()
			return nil
		})
	}()
	a.start(t)
	term := receive(t, terms, 5*time.Second, "fn at the first term")
	took := readLease(t, admin)

	// The first renewal, a third of the lease after the takeover, waits for the
	// lock for slowRenewal, then lands: a deadline counted from its answer would
	// come that much later than one counted from its send.
	release := lockLease(t, admin)
	var waited time.Duration
	waitFor(t, testLease, "the first renewal waiting for the lock", func() bool {
		waited = lockWait(t, admin)
		return waited >= slowRenewal
	})
	release()
	// Every later renewal waits for the lock until its statement timeout, and
	// fails. The lock is taken once the slow renewal committed.
	lockLease(t, admin)
	relocked := time.Now()
	if renewed := readLease(t, admin); renewed.epoch != took.epoch || renewed.expiresAt <= took.expiresAt {
		t.Fatalf("the slow renewal did not land: the lease expires at %d µs at epoch %d, and did at %d at %d", renewed.expiresAt, renewed.epoch, took.expiresAt, took.epoch)
	}

	cancelled(t, term, testLease, "fn's context once renewals stopped landing")
	seen, down := time.Now(), time.Since(relocked)
	if a.AmLeader() {
		t.Fatal("AmLeader is true once the term's context was cancelled")
	}
	row := readLease(t, admin)
	// The lease left as the read ran, and, the read's own time added back, as
	// the test saw the step-down, at most.
	left, atStepDown := row.left, row.left+time.Since(seen)
	t.Logf("the renewal waited %s for the lock; the leader stepped down %s after renewals stopped landing, with %s of its lease left", waited, down, atStepDown)
	switch {
	case left <= 0:
		t.Fatalf("the leader stepped down %s after its row expired, want before", -left)
	case atStepDown < testLease/3-slowRenewal/2:
		t.Errorf("the leader stepped down with %s of its lease left, want about a third: two thirds of the lease after its last renewal's send, not its answer, %s later", atStepDown, waited)
	case atStepDown > testLease/3+testLease/12:
		t.Errorf("the leader stepped down with %s of its lease left, want about a third: two thirds of the lease after its last renewal's send", atStepDown)
	case down > 2*testLease/3+testLease/12:
		t.Errorf("the leader stepped down %s after its renewals stopped landing, want within two thirds of the lease", down)
	}
	checkLogs(t, a)
}

// TestReleaseOnStop checks that a leader whose Run ends cancels its WhenLeader
// work, releases the lease, epoch kept, and deletes its registry row, so that
// the other replica leads well within the lease.
func TestReleaseOnStop(t *testing.T) {
	const lease = 6 * time.Second
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{lease: lease})
	b := newReplica(t, databaseURL, "replica-b", replicaConfig{lease: lease})
	terms := make(chan context.Context, 1)
	go func() {
		_ = a.WhenLeader(t.Context(), func(term context.Context) error {
			terms <- term
			<-term.Done()
			return nil
		})
	}()
	a.start(t)
	term := receive(t, terms, 5*time.Second, "replica-a's fn at its term")
	waitFor(t, 5*time.Second, "replica-a in the registry", func() bool { return registered(t, admin, a.id) })
	b.start(t)
	waitFor(t, 5*time.Second, "replica-b following replica-a", func() bool { return b.CurrentLeader() == a.addr })
	old := readLease(t, admin)

	stopped := time.Now()
	if err := a.stop(); err != nil {
		t.Fatalf("replica-a's Run: %v", err)
	}
	if term.Err() == nil {
		t.Error("replica-a's fn context is live once its Run returned")
	}
	if a.AmLeader() {
		t.Error("replica-a leads once its Run returned")
	}
	if registered(t, admin, a.id) {
		t.Error("replica-a's registry row survived its stop")
	}
	waitFor(t, lease/2, "replica-b leading after replica-a's stop", b.AmLeader)
	t.Logf("replica-b led %s after replica-a's stop", time.Since(stopped))
	if row := readLease(t, admin); row.holder != b.id || row.epoch != old.epoch+1 {
		t.Errorf("the lease is %s's at epoch %d, want %s's at %d: the release keeps the epoch", row.holder, row.epoch, b.id, old.epoch+1)
	}
	checkLogs(t, a)
}

// TestLeaseObserved checks that the Observer counts each renewal that landed,
// and each term lost before Run's end: one whose renewals stop landing, at its
// step-down, and one whose renewal finds the lease moved; a clean stop, which
// releases the lease, loses none.
func TestLeaseObserved(t *testing.T) {
	const lease = 1200 * time.Millisecond
	databaseURL, admin := migratedDatabase(t)
	var renewed, lost atomic.Int32
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{
		lease:    lease,
		poll:     20 * time.Millisecond,
		observer: Observer{Renewed: func() { renewed.Add(1) }, Lost: func() { lost.Add(1) }},
	})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)
	waitFor(t, 3*lease, "two renewals observed", func() bool { return renewed.Load() >= 2 })
	if n := lost.Load(); n != 0 {
		t.Fatalf("%d losses observed while replica-a renews, want none", n)
	}

	// Renewals stop landing: the step-down at the deadline is a loss.
	release := lockLease(t, admin)
	waitFor(t, 2*lease, "replica-a stepping down", func() bool { return !a.AmLeader() })
	waitFor(t, time.Second, "the step-down observed", func() bool { return lost.Load() == 1 })
	release()
	waitFor(t, 3*lease, "replica-a leading again", a.AmLeader)

	// A renewal that finds the lease moved is a loss too.
	before := renewed.Load()
	waitFor(t, 3*lease, "a renewal of the new term observed", func() bool { return renewed.Load() > before })
	moveLease(t, admin)
	waitFor(t, 2*lease, "the moved lease observed as lost", func() bool { return lost.Load() == 2 })

	waitFor(t, 3*lease, "replica-a leading again after the move", a.AmLeader)
	if err := a.stop(); err != nil {
		t.Fatalf("replica-a's Run: %v", err)
	}
	if n := lost.Load(); n != 2 {
		t.Errorf("%d losses observed after a clean stop, want the 2 before it: the release loses nothing", n)
	}
}

// TestNoReleaseWhileLeaderWorkRuns checks that a leader whose Run ends while
// its WhenLeader work has not returned, as the app's runners that outlived its
// drain deadline, does not release the lease: the row is left to expire, so
// that no other replica's leader-only work starts while this one's may still
// run, and the stop logs it. The other replica takes over once the row expired.
// Work that returns once its context is cancelled is waited for, and the lease
// released (TestReleaseOnStop).
func TestNoReleaseWhileLeaderWorkRuns(t *testing.T) {
	const lease = 1200 * time.Millisecond
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{lease: lease})
	stuck, running := make(chan struct{}), make(chan struct{}, 1)
	t.Cleanup(func() { close(stuck) })
	go func() {
		_ = a.WhenLeader(context.Background(), func(context.Context) error {
			running <- struct{}{}
			<-stuck // ignores its context, as a runner stuck in a call
			return nil
		})
	}()
	a.start(t)
	receive(t, running, 5*time.Second, "replica-a's work running in its term")
	taken := readLease(t, admin)

	if err := a.stop(); err != nil {
		t.Fatalf("replica-a's Run: %v", err)
	}
	row := readLease(t, admin)
	if row.left <= 0 || row.holder != a.id || row.epoch != taken.epoch {
		t.Fatalf("after the stop the lease is %s's at epoch %d, %s left; want replica-a's at %d, unreleased", row.holder, row.epoch, row.left, taken.epoch)
	}
	logs := a.logs.String()
	if !strings.Contains(logs, workNotEnded) || strings.Contains(logs, "database: released the lease") {
		t.Errorf("replica-a's stop logged no WARN of the work still running, or released the lease:\n%s", logs)
	}

	b := newReplica(t, databaseURL, "replica-b", replicaConfig{lease: lease, poll: 20 * time.Millisecond})
	b.start(t)
	waitFor(t, 2*lease, "replica-b leading once the row expired", b.AmLeader)
	if got := readLease(t, admin); got.epoch != taken.epoch+1 {
		t.Errorf("replica-b took the lease at epoch %d, want %d", got.epoch, taken.epoch+1)
	}
}

// repeatableReadDatabase is migratedDatabase on a database whose new sessions
// start at REPEATABLE READ, as an operator may set it for a database or a role.
// The sessions admin opened before keep their own default, so a transaction of
// admin's names its level.
func repeatableReadDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	databaseURL, admin := migratedDatabase(t)
	var name string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("read the database's name: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `ALTER DATABASE `+pgx.Identifier{name}.Sanitize()+
		` SET default_transaction_isolation = 'repeatable read'`); err != nil {
		t.Fatalf("set the database's default isolation: %v", err)
	}
	var level string
	if err := pgtest.Open(t, databaseURL).QueryRowContext(t.Context(), `SHOW default_transaction_isolation`).Scan(&level); err != nil {
		t.Fatalf("read a new session's default isolation: %v", err)
	}
	if level != "repeatable read" {
		t.Fatalf("a new session starts at %q, want %q", level, "repeatable read")
	}
	return databaseURL, admin
}

// TestLeaseStatementsRunAtReadCommitted checks that a statement onLease runs is
// a transaction at READ COMMITTED on a database whose sessions start at
// REPEATABLE READ: the level at which a statement that waited for the row's
// lock re-reads the row it left.
func TestLeaseStatementsRunAtReadCommitted(t *testing.T) {
	databaseURL, _ := repeatableReadDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{})
	var level string
	err := a.onLease(t.Context(), a.statementTimeout, func(ctx context.Context, conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&level)
	})
	if err != nil {
		t.Fatalf("a statement through onLease: %v", err)
	}
	if level != "read committed" {
		t.Errorf("a lease statement ran at %q, want %q", level, "read committed")
	}
}

// TestRenewalWaitingForATakeoverStepsDownAtOnce checks that a renewal that
// waited for a takeover in flight re-reads the row the takeover left, finds the
// lease moved and steps the leader down at once, with no statement failed, on a
// database whose sessions start at REPEATABLE READ, where the renewal would
// fail instead (SQLSTATE 40001) and the step-down wait for its retry. The
// takeover expires the lease and runs takeLease for replica-ghost in one
// transaction, left open between two of replica-a's renewals, so that no
// renewal lands between the expiry and the takeover; replica-a's next renewal
// is the one session that can wait for a lock.
func TestRenewalWaitingForATakeoverStepsDownAtOnce(t *testing.T) {
	// A lease of 6 s leaves the commit a second, the waiting renewal's
	// statement timeout.
	const lease = 6 * time.Second
	databaseURL, admin := repeatableReadDatabase(t)
	var lost atomic.Int32
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{
		lease:    lease,
		observer: Observer{Lost: func() { lost.Add(1) }},
	})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)
	old := readLease(t, admin)

	waitBetweenRenewals(t, admin, lease)
	ctx := context.Background()
	takeover, err := admin.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("begin the takeover: %v", err)
	}
	t.Cleanup(func() { _ = takeover.Rollback() })
	if _, err := takeover.ExecContext(ctx, `UPDATE podium_leader SET expires_at = now() WHERE id = 1`); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
	var epoch int64
	if err := takeover.QueryRowContext(ctx, takeLease,
		"replica-ghost", "replica-ghost.podium.test:7000", lease.Microseconds()).Scan(&epoch); err != nil {
		t.Fatalf("take the lease for replica-ghost: %v", err)
	}
	if epoch != old.epoch+1 {
		t.Fatalf("replica-ghost took the lease at epoch %d, want %d", epoch, old.epoch+1)
	}

	waitFor(t, lease/2, "replica-a's renewal waiting for the takeover", func() bool { return lockWaiters(t, admin) > 0 })
	if err := takeover.Commit(); err != nil {
		t.Fatalf("commit the takeover: %v", err)
	}
	// The step-down turns AmLeader false, then writes its line, then calls Lost.
	waitFor(t, 5*time.Second, "replica-a's term lost", func() bool { return lost.Load() > 0 })
	if a.AmLeader() {
		t.Error("replica-a still leads once its term was lost")
	}
	if n := lost.Load(); n != 1 {
		t.Errorf("Lost was called %d times, want 1", n)
	}
	logs := a.logs.String()
	if strings.Contains(logs, "renewing the lease failed") {
		t.Errorf("a renewal failed before the step-down, want none:\n%s", logs)
	}
	if !strings.Contains(logs, "a renewal found the lease moved to another replica") {
		t.Errorf("replica-a stepped down, but not as a renewal that found the lease moved:\n%s", logs)
	}
	checkLogs(t, a)
}
