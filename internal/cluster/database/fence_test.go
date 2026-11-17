package database

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

// fenceLease is the lease of the fence tests' replicas: long enough that a
// leader renews nothing while a test runs, its first renewal coming after ten
// seconds, so that it does not see a takeover by itself.
const fenceLease = 30 * time.Second

// fastPoll is the poll of a replica that must see a free lease at once.
const fastPoll = 20 * time.Millisecond

// testRole is a role to write.
func testRole(t *testing.T, id string) *model.Role {
	t.Helper()
	r, err := model.NewRole(id, "role "+id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// openStore opens a store on r's primary pool, fenced by r.
func openStore(t *testing.T, r *replica) *sqlstore.Store {
	t.Helper()
	s, err := sqlstore.Open(t.Context(), sqlstore.Options{Primary: r.db, Dialect: sqlstore.Postgres{}, Fence: r.Leadership})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return s
}

// TestExLeaderWriteFenced checks that a write transaction of a leader that has
// not yet seen another replica take the lease over fails the fence with
// cluster.ErrNotLeader, commits nothing, and steps the leader down at once.
func TestExLeaderWriteFenced(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{lease: fenceLease})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)
	storeA := openStore(t, a)
	old := readLease(t, admin)

	// The database sees replica-a's lease end, as a pause of its process would
	// leave it, unaware, and replica-b takes the lease over.
	expireLease(t, admin)
	b := newReplica(t, databaseURL, "replica-b", replicaConfig{lease: fenceLease, poll: fastPoll})
	b.start(t)
	waitFor(t, 5*time.Second, "replica-b leading", b.AmLeader)
	if !a.AmLeader() {
		t.Fatal("replica-a saw the takeover before its write: the test needs it unaware")
	}

	ctx := t.Context()
	err := storeA.UpsertRole(ctx, testRole(t, "role-1"))
	if !errors.Is(err, cluster.ErrNotLeader) || !errors.Is(err, errLeaseMoved) {
		t.Fatalf("the ex-leader's write: %v; want the fence's refusal of a moved lease, a cluster.ErrNotLeader", err)
	}
	if a.AmLeader() {
		t.Error("replica-a still leads once its write found the lease moved")
	}
	if _, err := storeA.GetRole(ctx, "role-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("reading the refused write back: %v, want storage.ErrNotFound", err)
	}

	// A later write is refused as one of a replica that does not hold the lease,
	// the other refusal; the holder writes.
	if err := storeA.UpsertRole(ctx, testRole(t, "role-2")); !errors.Is(err, cluster.ErrNotLeader) || !errors.Is(err, errNotHolder) {
		t.Errorf("the ex-leader's next write: %v; want the refusal of a replica that does not hold the lease", err)
	}
	if err := openStore(t, b).UpsertRole(ctx, testRole(t, "role-3")); err != nil {
		t.Errorf("the holder's write: %v", err)
	}
	if row := readLease(t, admin); row.holder != b.id || row.epoch != old.epoch+1 {
		t.Errorf("the lease is %s's at epoch %d, want %s's at %d", row.holder, row.epoch, b.id, old.epoch+1)
	}
}

// TestFenceRefusesAnotherHolderAtTheSameEpoch checks that the fence compares the
// holder as well as the epoch: after a database failover that restored an older
// row, another replica can hold the lease at the epoch this replica's term
// took, and two replicas must not pass one epoch. The write is refused with the
// moved lease's refusal, commits nothing, and steps the leader down.
func TestFenceRefusesAnotherHolderAtTheSameEpoch(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{lease: fenceLease})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)
	storeA := openStore(t, a)
	old := readLease(t, admin)

	// A plain UPDATE of holder, without FOR UPDATE first, breaks the invariant on
	// purpose: safe only while no fence is in flight, as here, before the write.
	if _, err := admin.ExecContext(t.Context(), `UPDATE podium_leader SET holder = 'replica-ghost' WHERE id = 1`); err != nil {
		t.Fatalf("hand the row to another holder: %v", err)
	}
	if row := readLease(t, admin); row.epoch != old.epoch || row.holder != "replica-ghost" {
		t.Fatalf("the lease is %s's at epoch %d, want replica-ghost's at the leader's %d", row.holder, row.epoch, old.epoch)
	}
	if !a.AmLeader() {
		t.Fatal("replica-a stepped down before its write: the test needs it unaware")
	}

	ctx := t.Context()
	err := storeA.UpsertRole(ctx, testRole(t, "role-1"))
	if !errors.Is(err, cluster.ErrNotLeader) || !errors.Is(err, errLeaseMoved) {
		t.Fatalf("the write under another holder at the same epoch: %v; want the fence's refusal of a moved lease, a cluster.ErrNotLeader", err)
	}
	if a.AmLeader() {
		t.Error("replica-a still leads once its write found another holder")
	}
	if _, err := storeA.GetRole(ctx, "role-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("reading the refused write back: %v, want storage.ErrNotFound", err)
	}
}

// TestTakeoverWaitsForAFencedWrite checks that the fence holds the lease row in
// key-share mode until the write transaction ends, and that a takeover, which
// locks the row FOR UPDATE first, waits for the write in flight, which commits,
// and takes the lease once it did. The lease it takes lasts a full lease from
// the end of its wait, not from the start of its statement, before it.
func TestTakeoverWaitsForAFencedWrite(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{lease: fenceLease})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)
	storeA := openStore(t, a)
	old := readLease(t, admin)
	expireLease(t, admin)

	ctx := t.Context()
	role := testRole(t, "role-1")
	fenced, proceed := make(chan struct{}), make(chan struct{})
	written := make(chan error, 1)
	go func() {
		written <- storeA.WithTx(ctx, func(tx storage.Storage) error {
			close(fenced)
			<-proceed
			return tx.UpsertRole(ctx, role)
		})
	}()
	receive(t, fenced, 5*time.Second, "the write past the fence")

	b := newReplica(t, databaseURL, "replica-b", replicaConfig{lease: fenceLease, poll: fastPoll})
	b.start(t)
	waitFor(t, 5*time.Second, "replica-b's takeover waiting for the lock", func() bool { return lockWaiters(t, admin) > 0 })
	if b.AmLeader() {
		t.Fatal("replica-b took the lease while a fenced write was in flight")
	}
	// The takeover waits a second by the database's clock, within its statement
	// timeout, a sixth of fenceLease.
	waitFor(t, 5*time.Second, "replica-b's takeover waiting a second for the lock", func() bool { return lockWait(t, admin) >= time.Second })
	close(proceed)
	if err := receive(t, written, 5*time.Second, "the fenced write"); err != nil {
		t.Fatalf("the fenced write: %v", err)
	}
	waitFor(t, 5*time.Second, "replica-b leading once the write committed", b.AmLeader)
	if left := readLease(t, admin).left; left <= fenceLease-500*time.Millisecond {
		t.Errorf("replica-b's lease has %s left once it leads, want over %s: a lease from the end of its wait, not from its statement's start, before it", left, fenceLease-500*time.Millisecond)
	}
	if _, err := storeA.GetRole(ctx, role.ID()); err != nil {
		t.Errorf("reading the fenced write back: %v", err)
	}
	if row := readLease(t, admin); row.holder != b.id || row.epoch != old.epoch+1 {
		t.Errorf("the lease is %s's at epoch %d, want %s's at %d", row.holder, row.epoch, b.id, old.epoch+1)
	}
}

// TestTakeoverYieldsToARenewalWhileItWaits checks that the holder's renewal
// lands while a takeover waits for a fenced write: the renewal's no-key update
// waits neither for the fence's key-share lock nor behind the takeover that
// waits for it. Once the write ends, the takeover finds the lease renewed and
// takes nothing.
func TestTakeoverYieldsToARenewalWhileItWaits(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	var renewed atomic.Int32
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{
		lease:    testLease,
		observer: Observer{Renewed: func() { renewed.Add(1) }},
	})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)
	b := newReplica(t, databaseURL, "replica-b", replicaConfig{lease: fenceLease, poll: fastPoll})
	b.start(t)
	waitFor(t, 5*time.Second, "replica-b following replica-a", func() bool { return b.CurrentLeader() == a.addr })
	storeA := openStore(t, a)
	old := readLease(t, admin)

	ctx := t.Context()
	role := testRole(t, "role-1")
	fenced, proceed := make(chan struct{}), make(chan struct{})
	written := make(chan error, 1)
	go func() {
		written <- storeA.WithTx(ctx, func(tx storage.Storage) error {
			close(fenced)
			<-proceed
			return tx.UpsertRole(ctx, role)
		})
	}()
	receive(t, fenced, 5*time.Second, "the write past the fence")

	// replica-b takes the lease once it reads it expired. A renewal of replica-a
	// that lands before replica-b reads the row leaves it nothing to take, so the
	// lease is expired again at each look that finds no takeover waiting. An
	// expiry that meets a renewal in flight while the takeover already waits
	// would queue behind the takeover: it gives up after 100 ms, and the next
	// look finds the takeover waiting.
	waitFor(t, 5*time.Second, "replica-b's takeover waiting for the lock", func() bool {
		if lockWaiters(t, admin) > 0 {
			return true
		}
		expireLeaseUnlessLocked(t, admin)
		return false
	})
	// A renewal lands while the takeover waits: the count grows, and the row's
	// lease lasts again, past the last expiry the test wrote.
	before, waiters := renewed.Load(), 0
	waitFor(t, testLease, "a renewal of replica-a landing while replica-b's takeover waits", func() bool {
		landed := renewed.Load() > before && readLease(t, admin).left > 0
		n := lockWaiters(t, admin)
		if n == 0 {
			t.Fatal("replica-b's takeover stopped waiting for the lock before a renewal of replica-a landed")
		}
		waiters = max(waiters, n)
		return landed
	})
	t.Logf("%d renewals of replica-a landed while replica-b's takeover waited %s for the lock; at most %d sessions waited for a lock", renewed.Load()-before, lockWait(t, admin), waiters)
	close(proceed)
	if err := receive(t, written, 5*time.Second, "the fenced write"); err != nil {
		t.Fatalf("the fenced write: %v", err)
	}

	waitFor(t, 5*time.Second, "replica-b following replica-a once its takeover found the lease renewed", func() bool { return b.CurrentLeader() == a.addr })
	if !a.AmLeader() || b.AmLeader() {
		t.Fatalf("replica-a leads %v and replica-b %v; want replica-a alone", a.AmLeader(), b.AmLeader())
	}
	if row := readLease(t, admin); row.holder != a.id || row.epoch != old.epoch {
		t.Errorf("the lease is %s's at epoch %d, want %s's at %d: the takeover took nothing", row.holder, row.epoch, a.id, old.epoch)
	}
}

// TestFenceWaitsForATakeoverInFlight checks that a fence that starts while a
// takeover holds the lease row waits for it, then reads the row it left: the
// takeover is the package's own statement, takeLease, run for another holder
// in a transaction left open, which took the expired lease at the next epoch,
// uncommitted. The write is refused with the moved lease's refusal, commits
// nothing, and steps the leader down.
func TestFenceWaitsForATakeoverInFlight(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{lease: fenceLease})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)
	storeA := openStore(t, a)
	old := readLease(t, admin)

	// The lease expires in the database's view, as a pause of replica-a's
	// process would leave it, unaware, and replica-ghost takes it over, in a
	// transaction left open.
	expireLease(t, admin)
	takeover, err := admin.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin the takeover: %v", err)
	}
	t.Cleanup(func() { _ = takeover.Rollback() })
	var epoch int64
	if err := takeover.QueryRowContext(context.Background(), takeLease,
		"replica-ghost", "replica-ghost.podium.test:7000", fenceLease.Microseconds()).Scan(&epoch); err != nil {
		t.Fatalf("take the lease for replica-ghost: %v", err)
	}
	if epoch != old.epoch+1 {
		t.Fatalf("replica-ghost took the lease at epoch %d, want %d", epoch, old.epoch+1)
	}

	ctx := t.Context()
	role := testRole(t, "role-1")
	written := make(chan error, 1)
	go func() { written <- storeA.UpsertRole(ctx, role) }()
	waitFor(t, 5*time.Second, "replica-a's fence waiting for the takeover", func() bool { return lockWaiters(t, admin) > 0 })
	select {
	case err := <-written:
		t.Fatalf("the write returned %v while the takeover was in flight, want it waiting", err)
	default:
	}

	if err := takeover.Commit(); err != nil {
		t.Fatalf("commit the takeover: %v", err)
	}
	err = receive(t, written, 5*time.Second, "the write once the takeover committed")
	if !errors.Is(err, cluster.ErrNotLeader) || !errors.Is(err, errLeaseMoved) {
		t.Fatalf("the write once the takeover committed: %v; want the fence's refusal of a moved lease, a cluster.ErrNotLeader", err)
	}
	if a.AmLeader() {
		t.Error("replica-a still leads once its write found the lease moved")
	}
	if _, err := storeA.GetRole(ctx, role.ID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("reading the refused write back: %v, want storage.ErrNotFound", err)
	}
}

// TestFencedWriteLongerThanTheLeaseKeepsTheLead checks that a write transaction
// that holds the fence's lock for three leases keeps the leadership: the
// renewals, no-key updates, do not wait for the fence's key-share lock, so the
// leader renews throughout, never steps down, and the write commits at the
// epoch it began in.
func TestFencedWriteLongerThanTheLeaseKeepsTheLead(t *testing.T) {
	const lease = 1200 * time.Millisecond
	databaseURL, admin := migratedDatabase(t)
	var renewed, lost atomic.Int32
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{
		lease:    lease,
		observer: Observer{Renewed: func() { renewed.Add(1) }, Lost: func() { lost.Add(1) }},
	})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)
	storeA := openStore(t, a)
	old := readLease(t, admin)

	ctx := t.Context()
	role := testRole(t, "role-1")
	fenced, proceed := make(chan struct{}), make(chan struct{})
	written := make(chan error, 1)
	go func() {
		written <- storeA.WithTx(ctx, func(tx storage.Storage) error {
			close(fenced)
			<-proceed
			return tx.UpsertRole(ctx, role)
		})
	}()
	receive(t, fenced, 5*time.Second, "the write past the fence")
	passed, before := time.Now(), renewed.Load()

	// For three leases from the moment the write passed the fence, replica-a
	// leads, and no renewal waits for a lock.
	for held := time.Since(passed); held < 3*lease; held = time.Since(passed) {
		if !a.AmLeader() {
			t.Fatalf("replica-a stepped down %s into a write that holds the fence's lock", held)
		}
		if n := lockWaiters(t, admin); n != 0 {
			t.Fatalf("%d sessions wait for a lock %s into the write, want none: a renewal waits for the fence's lock", n, held)
		}
		time.Sleep(waitStep)
	}
	// Nine renewals are due over three leases.
	if n := renewed.Load() - before; n < 4 {
		t.Fatalf("%d renewals landed over three leases of the write, want 4 at least", n)
	}
	close(proceed)
	if err := receive(t, written, 5*time.Second, "the fenced write"); err != nil {
		t.Fatalf("the fenced write: %v", err)
	}
	if _, err := storeA.GetRole(ctx, role.ID()); err != nil {
		t.Errorf("reading the fenced write back: %v", err)
	}
	if n := lost.Load(); n != 0 {
		t.Errorf("%d losses observed, want none", n)
	}
	if row := readLease(t, admin); row.holder != a.id || row.epoch != old.epoch {
		t.Errorf("the lease is %s's at epoch %d, want %s's at %d", row.holder, row.epoch, a.id, old.epoch)
	}
}

// TestFollowerWriteWithoutLeaderIs503 checks Review Focus 2: while a failover
// runs and no leader is known, a follower's store refuses a write with
// cluster.ErrNotLeader and writes nothing, and the HTTP layer answers 503, with
// no leader to name.
func TestFollowerWriteWithoutLeaderIs503(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	// The leader is gone: its lease expired, and the takeover waits for a lock.
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO podium_leader (id, epoch, holder, address, expires_at)
VALUES (1, 7, 'replica-gone', 'replica-gone.podium.test:7000', now() - interval '1 second')`); err != nil {
		t.Fatalf("insert the gone leader's lease: %v", err)
	}
	release := lockLease(t, admin)
	b := newReplica(t, databaseURL, "replica-b", replicaConfig{lease: fenceLease, poll: fastPoll})
	b.start(t)
	waitFor(t, 5*time.Second, "replica-b's takeover waiting for the lock", func() bool { return lockWaiters(t, admin) > 0 })
	if b.AmLeader() || b.CurrentLeader() != "" {
		t.Fatalf("replica-b leads %v and knows the leader %q; want a follower that knows no leader", b.AmLeader(), b.CurrentLeader())
	}

	ctx := t.Context()
	store := openStore(t, b)
	role := testRole(t, "role-1")
	if err := store.UpsertRole(ctx, role); !errors.Is(err, cluster.ErrNotLeader) {
		t.Fatalf("the follower's write: %v, want cluster.ErrNotLeader", err)
	}

	// Through the HTTP layer: a GET that writes, as a session rotation does,
	// passes the Leader middleware of a follower and meets the store's refusal;
	// a POST, the middleware holds for a leader, up to its handover wait, and
	// then refuses itself, as the takeover still waits for the lock. Both answer
	// 503, naming no leader.
	writes := middleware.Leader(b, middleware.LeaderOptions{ForwardPort: 8080, HandoverWait: 200 * time.Millisecond})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := store.UpsertRole(r.Context(), role); err != nil {
			response.FromError(w, r, httpctx.RenderBlock, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := httptest.NewRecorder()
		writes.ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/roles", nil))
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get(response.LeaderHeader) != "" {
			t.Errorf("a %s that writes on the follower: HTTP %d with %s %q, want 503 without it", method, rec.Code, response.LeaderHeader, rec.Header().Get(response.LeaderHeader))
		}
	}
	if _, err := store.GetRole(ctx, role.ID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("reading the refused writes back: %v, want storage.ErrNotFound", err)
	}

	// Once the lock is gone, the takeover completes, and the write lands.
	release()
	waitFor(t, 5*time.Second, "replica-b leading", b.AmLeader)
	if row := readLease(t, admin); row.holder != b.id || row.epoch != 8 {
		t.Errorf("the lease is %s's at epoch %d, want %s's at 8", row.holder, row.epoch, b.id)
	}
	if err := store.UpsertRole(ctx, role); err != nil {
		t.Errorf("the write once replica-b leads: %v", err)
	}
}
