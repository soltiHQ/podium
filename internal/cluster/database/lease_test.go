package database

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// moveLease hands the lease to another replica at the next epoch, expired at
// once, so that it is free to take again.
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
	// The takeover's now() is its expires_at less the lease.
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
