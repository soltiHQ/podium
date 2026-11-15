package database

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
// share mode until the write transaction ends: a takeover waits for the write in
// flight, which commits, and takes the lease once it did.
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
	close(proceed)
	if err := receive(t, written, 5*time.Second, "the fenced write"); err != nil {
		t.Fatalf("the fenced write: %v", err)
	}
	waitFor(t, 5*time.Second, "replica-b leading once the write committed", b.AmLeader)
	if _, err := storeA.GetRole(ctx, role.ID()); err != nil {
		t.Errorf("reading the fenced write back: %v", err)
	}
	if row := readLease(t, admin); row.holder != b.id || row.epoch != old.epoch+1 {
		t.Errorf("the lease is %s's at epoch %d, want %s's at %d", row.holder, row.epoch, b.id, old.epoch+1)
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
	// a POST, the middleware refuses itself. Both answer 503, naming no leader.
	writes := middleware.Leader(b, middleware.LeaderOptions{ForwardPort: 8080})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
