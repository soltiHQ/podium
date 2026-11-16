package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster/database"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// The work of podium drop-schema, DropSchema, next to a running replica: with
// no wait it refuses at once with database.ErrRunning, and drops nothing; with
// a wait it waits while the replica runs, logging one WARN, and once the
// replica stopped, which deletes its registry row and releases the lease, it
// drops every table of the schema, logging where and what; then it finds
// nothing to drop. No line carries the URL's secrets.
func TestDatabaseDropSchema(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	db := pgtest.Open(t, databaseURL)
	ctx := context.Background()
	n := databaseNodes(t, databaseURL, "127.0.0.1")[0]
	n.start(t)
	waitFor(t, 10*time.Second, "the replica leads", n.app.leadership.AmLeader)
	cfg := n.cfg
	var logs syncBuffer
	logger := zerolog.New(&logs)
	tableCount := func() int {
		t.Helper()
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name LIKE 'podium\_%'`).Scan(&count); err != nil {
			t.Fatalf("count the tables: %v", err)
		}
		return count
	}
	before := tableCount()
	if before == 0 {
		t.Fatal("the replica's start left no table of podium's")
	}

	begin := time.Now()
	if err := DropSchema(ctx, cfg, logger, 0, withDatabaseTimings(dbTimings)); !errors.Is(err, database.ErrRunning) {
		t.Fatalf("DropSchema without a wait while the replica runs: %v; want database.ErrRunning", err)
	}
	if took := time.Since(begin); took >= dbTimings.retry {
		t.Errorf("the refusal took %s, want at once", took)
	}
	if got := tableCount(); got != before {
		t.Fatalf("%d tables after the refusal, want %d, untouched", got, before)
	}

	done := make(chan error, 1)
	go func() { done <- DropSchema(ctx, cfg, logger, 30*time.Second, withDatabaseTimings(dbTimings)) }()
	waitFor(t, 10*time.Second, "the drop waits for the replica", func() bool {
		return strings.Contains(logs.String(), "database: replicas still run on the database: waiting for them to stop")
	})
	select {
	case err := <-done:
		t.Fatalf("DropSchema returned while the replica runs: %v", err)
	default:
	}
	if got := tableCount(); got != before {
		t.Fatalf("%d tables while the drop waits, want %d, untouched", got, before)
	}
	n.stop()
	if err := receiveErr(t, done, 15*time.Second); err != nil {
		t.Fatalf("DropSchema once the replica stopped: %v", err)
	}
	if got := tableCount(); got != 0 {
		t.Errorf("%d tables of podium's after the drop, want none", got)
	}
	if v, err := sqlstore.SchemaVersion(ctx, db); err != nil || v != 0 {
		t.Errorf("SchemaVersion after the drop = %d, %v; want 0", v, err)
	}
	out := logs.String()
	for _, want := range []string{`"message":"database: dropped the schema"`, `"podium_agents"`, `"database":"` + databaseName(t, databaseURL) + `"`, `"host":"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %s:\n%s", want, out)
		}
	}
	if waits := strings.Count(out, "waiting for them to stop"); waits != 1 {
		t.Errorf("%d WARN lines about the wait, want one:\n%s", waits, out)
	}

	if err := DropSchema(ctx, cfg, logger, 0, withDatabaseTimings(dbTimings)); err != nil {
		t.Fatalf("DropSchema on a dropped schema: %v", err)
	}
	if !strings.Contains(logs.String(), "database: no schema to drop") {
		t.Errorf("the log lacks the second run's line:\n%s", logs.String())
	}
	noSecrets(t, urlSecrets(t, databaseURL), map[string]string{"the log": logs.String()})
}

// The work of podium drop-schema while another session holds a lock on a table
// of the schema, as pg_dump does, and no replica runs: with no wait it refuses
// with sqlstore.ErrLocked once its lock timeout passed, and drops nothing; with
// a wait it waits for the lock, logging one WARN that says so, and drops once
// the lock is free.
func TestDatabaseDropSchemaWaitsForALock(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	db := pgtest.Open(t, databaseURL)
	ctx := context.Background()
	if err := sqlstore.Migrate(ctx, db, sqlstore.Postgres{}); err != nil {
		t.Fatal(err)
	}
	holder, err := pgtest.Open(t, databaseURL).BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `LOCK TABLE podium_agents IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	cfg := databaseNodes(t, databaseURL, "127.0.0.1")[0].cfg
	var logs syncBuffer
	logger := zerolog.New(&logs)
	// The startup retry outlasts the drop's lock timeout of 5 s, as in
	// production, where it is 30 s: a lock wait ends in ErrLocked, not in the
	// attempt's deadline.
	timings := dbTimings
	timings.retry = 10 * time.Second

	if err := DropSchema(ctx, cfg, logger, 0, withDatabaseTimings(timings)); !errors.Is(err, sqlstore.ErrLocked) {
		t.Fatalf("DropSchema without a wait while a lock is held: %v; want sqlstore.ErrLocked", err)
	}
	if v, err := sqlstore.SchemaVersion(ctx, db); err != nil || v < 1 {
		t.Fatalf("the schema after the refusal: %d, %v; want it untouched", v, err)
	}

	done := make(chan error, 1)
	go func() { done <- DropSchema(ctx, cfg, logger, 60*time.Second, withDatabaseTimings(timings)) }()
	waitFor(t, 20*time.Second, "the drop waits for the lock", func() bool {
		return strings.Contains(logs.String(), "database: another session holds a lock on a table of podium's schema: waiting for it")
	})
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := receiveErr(t, done, 20*time.Second); err != nil {
		t.Fatalf("DropSchema once the lock is free: %v", err)
	}
	if v, err := sqlstore.SchemaVersion(ctx, db); err != nil || v != 0 {
		t.Errorf("SchemaVersion after the drop = %d, %v; want 0", v, err)
	}
	if waits := strings.Count(logs.String(), "waiting for it"); waits != 1 {
		t.Errorf("%d WARN lines about the lock, want one:\n%s", waits, logs.String())
	}
	noSecrets(t, urlSecrets(t, databaseURL), map[string]string{"the log": logs.String()})
}
