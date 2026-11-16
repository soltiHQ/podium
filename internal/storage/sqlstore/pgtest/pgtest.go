// Package pgtest hands each test of the database mode a fresh Postgres database
// of its own, on the server that PODIUM_TEST_POSTGRES_URL names. A test that
// calls it skips while the variable is unset, so `go test ./...` runs without
// Postgres; the Task gate ci/golang/test/postgres starts a server and sets the
// variable. It is a package for tests only.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	// The pgx driver of database/sql, as "pgx".
	_ "github.com/jackc/pgx/v5/stdlib"
)

// EnvURL names the variable that holds the connection URL of the Postgres
// server the tests use, postgres://user:password@host:port/database. Its user
// must be allowed to create databases. pgtest never prints the URL, which holds
// a password; the driver's errors it prints redact it.
const EnvURL = "PODIUM_TEST_POSTGRES_URL"

// cleanupTimeout bounds the statements of a cleanup, which runs once the
// test's context is done.
const cleanupTimeout = 30 * time.Second

// NewDatabase creates an empty database on the server EnvURL names, drops it
// when t ends, and returns its connection URL. It skips t while EnvURL is
// unset.
func NewDatabase(t testing.TB) string {
	t.Helper()
	server := os.Getenv(EnvURL)
	if server == "" {
		t.Skipf("%s is unset: no Postgres server to test against", EnvURL)
	}
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("pgtest: %s is not a postgres:// URL", EnvURL)
	}

	random := make([]byte, 8)
	_, _ = rand.Read(random)
	name := "podium_test_" + hex.EncodeToString(random)
	if err := onServer(t.Context(), server, "CREATE DATABASE "+name+" TEMPLATE template0"); err != nil {
		t.Fatalf("pgtest: create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		// FORCE ends the sessions a test left open.
		if err := onServer(ctx, server, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("pgtest: drop database %s: %v", name, err)
		}
	})

	u.Path, u.RawPath = "/"+name, ""
	return u.String()
}

// Open opens a pool on the database at databaseURL, checks that it answers, and
// closes the pool when t ends.
func Open(t testing.TB, databaseURL string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("pgtest: open the database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("pgtest: reach the database: %v", err)
	}
	return db
}

// Fresh returns a pool on a fresh empty database: NewDatabase, then Open.
func Fresh(t testing.TB) *sql.DB {
	t.Helper()
	return Open(t, NewDatabase(t))
}

// onServer runs statement on the database of the server URL, outside a
// transaction, as CREATE DATABASE and DROP DATABASE need.
func onServer(ctx context.Context, server, statement string) error {
	db, err := sql.Open("pgx", server)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(ctx, statement)
	return err
}
