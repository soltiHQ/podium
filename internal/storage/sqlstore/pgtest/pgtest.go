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
	"strings"
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

// DMLRole creates a role that may log in to the database at databaseURL and
// read and write the rows of its tables, and nothing else, as a deployment
// whose migrations a tool of its own applies runs podium: CONNECT on the
// database, USAGE on schema public, and SELECT, INSERT, UPDATE and DELETE on
// the tables of schema public, those there now and, as default privileges of
// databaseURL's user, those that user makes later. It grants nothing on a
// sequence. It returns databaseURL with the role's name and a password of its
// own, and drops the role when t ends, before NewDatabase drops the database.
func DMLRole(t testing.TB, databaseURL string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal("pgtest: the database URL does not parse")
	}
	random := make([]byte, 24)
	_, _ = rand.Read(random)
	name := "podium_dml_" + hex.EncodeToString(random[:8])
	password := hex.EncodeToString(random[8:])
	database := strings.TrimPrefix(u.Path, "/")

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("pgtest: open the database: %v", err)
	}
	defer func() { _ = db.Close() }()
	// The statements name the role and the database, never the password, which
	// the first one carries.
	for i, statement := range []string{
		"CREATE ROLE " + name + " LOGIN PASSWORD '" + password + "'",
		"GRANT CONNECT ON DATABASE " + database + " TO " + name,
		"GRANT USAGE ON SCHEMA public TO " + name,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO " + name,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO " + name,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			if i == 0 {
				statement = "CREATE ROLE " + name
			}
			t.Fatalf("pgtest: %s: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		// DROP OWNED revokes what the role holds in the database, its default
		// privileges and its CONNECT included, without which DROP ROLE refuses.
		for _, statement := range []string{"DROP OWNED BY " + name, "DROP ROLE " + name} {
			if err := onServer(ctx, databaseURL, statement); err != nil {
				t.Errorf("pgtest: %s: %v", statement, err)
				return
			}
		}
	})
	u.User = url.UserPassword(name, password)
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
