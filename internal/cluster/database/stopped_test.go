package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// checkStopped runs CheckStopped in a transaction of its own on db.
func checkStopped(t *testing.T, db *sql.DB) error {
	t.Helper()
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	return CheckStopped(ctx, tx)
}

// TestCheckStopped reads whether a replica of podium runs on the database: one
// that heartbeated its registry row within three heartbeats, or a lease that
// has not expired, by the database's clock, runs, and CheckStopped answers
// ErrRunning, naming what it found; a row unseen for longer, an expired or a
// released lease, no row and no table are no replica.
func TestCheckStopped(t *testing.T) {
	const (
		replicaSeen = `INSERT INTO podium_replicas (id, advertise_addr, digests, last_seen) VALUES ($1, 'replica.podium.test:7000', '\x00', now() - $2::interval)`
		leaseUntil  = `INSERT INTO podium_leader (id, epoch, holder, address, expires_at) VALUES (1, 1, 'replica-a', 'replica.podium.test:7000', now() + $1::interval)`
	)
	type statement struct {
		sql  string
		args []any
	}
	for _, tc := range []struct {
		name       string
		schema     bool
		statements []statement
		says       string // what the refusal names; "" for no replica
	}{
		{name: "no schema"},
		{name: "no rows", schema: true},
		{name: "a replica seen now", schema: true, statements: []statement{{replicaSeen, []any{"replica-a", "0 s"}}}, says: "1 replica heartbeated within 3s"},
		{name: "two replicas seen within three heartbeats", schema: true, statements: []statement{
			{replicaSeen, []any{"replica-a", "0 s"}},
			{replicaSeen, []any{"replica-b", "2 s"}},
		}, says: "2 replicas heartbeated within 3s"},
		{name: "a replica unseen for a minute", schema: true, statements: []statement{{replicaSeen, []any{"replica-a", "1 min"}}}},
		{name: "a held lease", schema: true, statements: []statement{{leaseUntil, []any{"1 min"}}}, says: "the lease is held"},
		{name: "an expired lease", schema: true, statements: []statement{{leaseUntil, []any{"-1 s"}}}},
		{name: "a replica and a held lease", schema: true, statements: []statement{
			{replicaSeen, []any{"replica-a", "0 s"}},
			{leaseUntil, []any{"1 min"}},
		}, says: "1 replica heartbeated within 3s, and the lease is held"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var db *sql.DB
			if tc.schema {
				_, db = migratedDatabase(t)
			} else {
				db = pgtest.Fresh(t)
			}
			for _, s := range tc.statements {
				if _, err := db.ExecContext(t.Context(), s.sql, s.args...); err != nil {
					t.Fatalf("%s: %v", s.sql, err)
				}
			}
			err := checkStopped(t, db)
			switch {
			case tc.says == "" && err != nil:
				t.Errorf("CheckStopped: %v; want nil, no replica runs", err)
			case tc.says != "" && (!errors.Is(err, ErrRunning) || !strings.Contains(err.Error(), tc.says)):
				t.Errorf("CheckStopped: %v; want ErrRunning saying %q", err, tc.says)
			}
		})
	}
}

// TestCheckStoppedWithAReplica runs a replica: while it leads, CheckStopped
// answers ErrRunning, and once it stopped, which deletes its registry row and
// releases the lease, nil at once.
func TestCheckStoppedWithAReplica(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	r := newReplica(t, databaseURL, "replica-a", replicaConfig{})
	r.start(t)
	waitFor(t, 10*time.Second, "the replica leads", r.AmLeader)
	if err := checkStopped(t, admin); !errors.Is(err, ErrRunning) {
		t.Fatalf("CheckStopped while the replica leads: %v; want ErrRunning", err)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if err := checkStopped(t, admin); err != nil {
		t.Errorf("CheckStopped once the replica stopped: %v; want nil at once", err)
	}
}

// CheckStopped reads through the transaction it is given: what that
// transaction wrote, uncommitted, counts.
func TestCheckStoppedReadsTheTransaction(t *testing.T) {
	_, db := migratedDatabase(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO podium_replicas (id, advertise_addr, digests, last_seen) VALUES ('replica-a', 'replica.podium.test:7000', '\x00', now())`); err != nil {
		t.Fatal(err)
	}
	if err := CheckStopped(ctx, tx); !errors.Is(err, ErrRunning) {
		t.Errorf("CheckStopped: %v; want ErrRunning for the row the transaction wrote", err)
	}
}
