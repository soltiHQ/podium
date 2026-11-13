package sqlstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

func TestPostgresName(t *testing.T) {
	if got := (Postgres{}).Name(); got != "postgres" {
		t.Errorf("Name = %q, want postgres", got)
	}
}

func TestRebind(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{`SELECT 1`, `SELECT 1`},
		{`SELECT id FROM t WHERE a = ? AND b = ?`, `SELECT id FROM t WHERE a = $1 AND b = $2`},
		{`INSERT INTO t VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, `INSERT INTO t VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`},
		// A ? in a string literal or a quoted identifier is no placeholder.
		{`SELECT '?' FROM t WHERE a = ?`, `SELECT '?' FROM t WHERE a = $1`},
		{`SELECT 'it''s ?' FROM "a?b" WHERE c = ?`, `SELECT 'it''s ?' FROM "a?b" WHERE c = $1`},
		{`SELECT "say ""?""" FROM t WHERE a = ?`, `SELECT "say ""?""" FROM t WHERE a = $1`},
	} {
		if got := (Postgres{}).Rebind(tc.query); got != tc.want {
			t.Errorf("Rebind(%q) = %q, want %q", tc.query, got, tc.want)
		}
	}
}

// secret is a value a test puts where a careless classification would copy it:
// in a row, in PgError.Detail and Message, in a statement's argument, in a
// connection string.
const secret = "secret-value-1729"

// TestClassifyMapping classifies the errors pgx and database/sql return,
// without a database, as the store does: by SQLSTATE and error type, the error
// of a caller's done context as itself, and nothing of the error's text but the
// SQLSTATE and the identifiers of the table, column and constraint.
func TestClassifyMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		done bool // the caller's context is done
		want error
		says []string
	}{
		{name: "unique violation", err: &pgconn.PgError{
			Code: "23505", Message: `duplicate key value violates unique constraint "t_pkey"`,
			Detail: "Key (name)=(" + secret + ") already exists.", TableName: "t", ConstraintName: "t_pkey",
		}, want: storage.ErrAlreadyExists, says: []string{"23505", `table "t"`, `constraint "t_pkey"`}},
		{name: "serialization failure", err: &pgconn.PgError{Code: "40001", Message: "could not serialize access"}, want: storage.ErrUnavailable, says: []string{"40001"}},
		{name: "deadlock", err: &pgconn.PgError{Code: "40P01", Message: "deadlock detected", Detail: secret}, want: storage.ErrUnavailable, says: []string{"40P01"}},
		{name: "a write to a read-only server", err: &pgconn.PgError{Code: "25006", Message: "cannot execute INSERT in a read-only transaction"}, want: storage.ErrUnavailable, says: []string{"25006"}},
		{name: "a statement timeout, the caller's context live", err: &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}, want: storage.ErrUnavailable, says: []string{"57014"}},
		{name: "a cancelled statement, the caller's context done", err: &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}, done: true, want: context.Canceled},
		{name: "a lock timeout", err: &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}, want: storage.ErrUnavailable, says: []string{"55P03"}},
		{name: "insufficient resources", err: &pgconn.PgError{Code: "53000"}, want: storage.ErrUnavailable, says: []string{"53000"}},
		{name: "disk full", err: &pgconn.PgError{Code: "53100", Message: "could not extend file"}, want: storage.ErrUnavailable, says: []string{"53100"}},
		{name: "out of memory", err: &pgconn.PgError{Code: "53200", Message: "out of memory"}, want: storage.ErrUnavailable, says: []string{"53200"}},
		{name: "too many connections", err: &pgconn.PgError{Code: "53300", Message: "sorry, too many clients already"}, want: storage.ErrUnavailable, says: []string{"53300"}},
		{name: "a configuration limit exceeded", err: &pgconn.PgError{Code: "53400"}, want: storage.ErrUnavailable, says: []string{"53400"}},
		{name: "connection failure", err: &pgconn.PgError{Code: "08006"}, want: storage.ErrUnavailable, says: []string{"08006"}},
		{name: "no connection", err: &pgconn.PgError{Code: "08003"}, want: storage.ErrUnavailable, says: []string{"08003"}},
		{name: "administrator shutdown", err: &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}, want: storage.ErrUnavailable, says: []string{"57P01"}},
		{name: "crash shutdown", err: &pgconn.PgError{Code: "57P02"}, want: storage.ErrUnavailable, says: []string{"57P02"}},
		{name: "cannot connect now", err: &pgconn.PgError{Code: "57P03"}, want: storage.ErrUnavailable, says: []string{"57P03"}},
		{name: "an idle-session timeout", err: &pgconn.PgError{Code: "57P05", Message: "terminating connection due to idle-session timeout"}, want: storage.ErrUnavailable, says: []string{"57P05"}},
		{name: "a dropped database", err: &pgconn.PgError{Code: "57P04", Message: "terminating connection because of dropped database"}, want: storage.ErrInternal, says: []string{"57P04"}},
		{name: "another SQLSTATE", err: &pgconn.PgError{
			Code: "22P02", Message: `invalid input syntax for type integer: "` + secret + `"`, ColumnName: "n",
		}, want: storage.ErrInternal, says: []string{"22P02", `column "n"`}},
		{name: "a bad connection", err: fmt.Errorf("exec: %w", driver.ErrBadConn), want: storage.ErrUnavailable},
		{name: "a refused dial", err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, want: storage.ErrUnavailable},
		{name: "a stream cut short", err: fmt.Errorf("receive message: %w", io.ErrUnexpectedEOF), want: storage.ErrUnavailable},
		{name: "a closed connection", err: fmt.Errorf("query: %w", pgconn.ErrConnClosed), want: storage.ErrUnavailable},
		{name: "an unknown error", err: errors.New("sql: Scan error on column index 0: " + secret), want: storage.ErrInternal},
		{name: "an error already classified", err: fmt.Errorf("%w: gone", storage.ErrNotFound), want: storage.ErrNotFound},
		// The sentinel does not shelter the server's error: it is classified by its
		// SQLSTATE, and its Detail goes no further.
		{name: "a sentinel that wraps a server error", err: fmt.Errorf("%w: %w", storage.ErrNotFound, &pgconn.PgError{
			Code: "22P02", Message: `invalid input syntax for type integer: "` + secret + `"`, Detail: "Key (id)=(" + secret + ")",
		}), want: storage.ErrInternal, says: []string{"22P02"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.done {
				cancel()
			}
			got := classify(ctx, Postgres{}, tc.err)
			if !errors.Is(got, tc.want) {
				t.Fatalf("classify = %v, want %v", got, tc.want)
			}
			if tc.done && got != ctx.Err() {
				t.Errorf("classify = %v, want the done context's own error %v", got, ctx.Err())
			}
			says := got.Error()
			for _, s := range tc.says {
				if !strings.Contains(says, s) {
					t.Errorf("classify = %q, want it to name %s", says, s)
				}
			}
			for _, leak := range []string{
				secret, "Key (", "duplicate key value", "invalid input syntax", "terminating connection",
				"canceling statement", "read-only transaction", "too many clients", "could not extend", "out of memory",
			} {
				if strings.Contains(says, leak) {
					t.Errorf("classify = %q, carries %q", says, leak)
				}
			}
		})
	}

	t.Run("a done context", func(t *testing.T) {
		for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
			if got := (Postgres{}).Classify(fmt.Errorf("query: %w", ctxErr)); got != ctxErr {
				t.Errorf("Classify of an error of %v = %v, want %v itself", ctxErr, got, ctxErr)
			}
		}
		if got := (Postgres{}).Classify(nil); got != nil {
			t.Errorf("Classify(nil) = %v, want nil", got)
		}
	})
}

// TestClassify classifies the errors a real driver and a real server return: a
// unique violation, a refused connection and one that times out, a
// serialization failure, a deadlock, a write to a read-only transaction, a
// statement timeout, a statement whose caller's context ends, a lock timeout
// and a connection the server ended.
func TestClassify(t *testing.T) {
	ctx := t.Context()
	// classified checks what Classify makes of err.
	classified := func(t *testing.T, err error, want error, says string) {
		t.Helper()
		if err == nil {
			t.Fatalf("no error, want one Classify makes %v of", want)
		}
		got := Postgres{}.Classify(err)
		if !errors.Is(got, want) {
			t.Fatalf("Classify(%v) = %v, want %v", err, got, want)
		}
		if !strings.Contains(got.Error(), says) {
			t.Errorf("Classify = %q, want it to name %s", got, says)
		}
		for _, leak := range []string{secret, "Key (", "user="} {
			if strings.Contains(got.Error(), leak) {
				t.Errorf("Classify = %q, carries %q", got, leak)
			}
		}
	}

	t.Run("connection refused", func(t *testing.T) {
		// Nothing listens on port 1 of the loopback address.
		db, err := sql.Open("pgx", "postgres://podium:"+secret+"@127.0.0.1:1/podium?sslmode=disable&connect_timeout=5")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		classified(t, db.PingContext(ctx), storage.ErrUnavailable, "")
	})

	t.Run("a connection attempt that times out", func(t *testing.T) {
		// A server that takes the connection and never answers: the attempt
		// ends at connect_timeout, which pgx runs as a context deadline of its
		// own. The database is unreachable, not the caller out of time.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
			}
		}()
		db, err := sql.Open("pgx", "postgres://podium:"+secret+"@"+ln.Addr().String()+"/podium?sslmode=disable&connect_timeout=1")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		err = db.PingContext(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ping: %v, want an error of a deadline", err)
		}
		classified(t, err, storage.ErrUnavailable, "")
	})

	t.Run("a connection the server refuses", func(t *testing.T) {
		// The server's refusal travels in the connection error: the store names
		// its SQLSTATE and nothing else, neither the message nor the user, the
		// host or the password the URL carries.
		server := os.Getenv(pgtest.EnvURL)
		if server == "" {
			t.Skipf("%s is unset: no Postgres server to test against", pgtest.EnvURL)
		}
		base, err := url.Parse(server)
		if err != nil || base.User == nil {
			t.Fatalf("pgtest: %s is not a postgres:// URL with a user", pgtest.EnvURL)
		}
		for _, tc := range []struct {
			name, sqlstate string
			change         func(*url.URL)
		}{
			{"a wrong password", "28P01", func(u *url.URL) { u.User = url.UserPassword(u.User.Username(), secret) }},
			{"an unknown database", "3D000", func(u *url.URL) { u.Path = "/no_such_database_" + secret }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				u := *base
				tc.change(&u)
				db, err := sql.Open("pgx", u.String())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				err = db.PingContext(ctx)
				if err == nil {
					t.Skip("the server accepted the connection: it does not check this")
				}
				classified(t, err, storage.ErrUnavailable, "SQLSTATE "+tc.sqlstate)
				got := Postgres{}.Classify(err).Error()
				for _, leak := range []string{u.Hostname(), "FATAL", "authentication failed", "does not exist"} {
					if strings.Contains(got, leak) {
						t.Errorf("Classify = %q, carries %q", got, leak)
					}
				}
			})
		}
	})

	t.Run("unique violation", func(t *testing.T) {
		db := pgtest.Fresh(t)
		insert := func() error {
			_, err := db.ExecContext(ctx, Postgres{}.Rebind(`INSERT INTO names (name) VALUES (?)`), secret)
			return err
		}
		if _, err := db.ExecContext(ctx, `CREATE TABLE names (name text PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		if err := insert(); err != nil {
			t.Fatal(err)
		}
		classified(t, insert(), storage.ErrAlreadyExists, `constraint "names_pkey"`)
	})

	t.Run("serialization failure", func(t *testing.T) {
		db := pgtest.Fresh(t)
		if _, err := db.ExecContext(ctx, `CREATE TABLE counters (id int PRIMARY KEY, n int NOT NULL); INSERT INTO counters VALUES (1, 0)`); err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		var n int
		// The first read takes the transaction's snapshot; a write commits after it.
		if err := tx.QueryRowContext(ctx, `SELECT n FROM counters WHERE id = 1`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE counters SET n = n + 1 WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE counters SET n = n + 1 WHERE id = 1`)
		classified(t, err, storage.ErrUnavailable, "40001")
	})

	t.Run("deadlock", func(t *testing.T) {
		db := pgtest.Fresh(t)
		if _, err := db.ExecContext(ctx, `CREATE TABLE counters (id int PRIMARY KEY, n int NOT NULL); INSERT INTO counters VALUES (1, 0), (2, 0)`); err != nil {
			t.Fatal(err)
		}
		// Each transaction locks one row, then wants the other's: the server
		// breaks the cycle, after deadlock_timeout, by failing one of them.
		txs := make([]*sql.Tx, 2)
		for i := range txs {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(ctx, `UPDATE counters SET n = n + 1 WHERE id = $1`, i+1); err != nil {
				t.Fatal(err)
			}
			txs[i] = tx
		}
		errs := make(chan error, len(txs))
		for i, tx := range txs {
			go func() {
				_, err := tx.ExecContext(ctx, `UPDATE counters SET n = n + 1 WHERE id = $1`, 2-i)
				errs <- err
			}()
		}
		var failed []error
		for range txs {
			if err := <-errs; err != nil {
				failed = append(failed, err)
			}
		}
		if len(failed) != 1 {
			t.Fatalf("%d transactions failed, want one: %v", len(failed), failed)
		}
		classified(t, failed[0], storage.ErrUnavailable, "40P01")
	})

	t.Run("a write to a read-only transaction", func(t *testing.T) {
		db := pgtest.Fresh(t)
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `CREATE TABLE names (name text PRIMARY KEY)`)
		classified(t, err, storage.ErrUnavailable, "25006")
	})

	t.Run("a statement timeout", func(t *testing.T) {
		db := pgtest.Fresh(t)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '50ms'`); err != nil {
			t.Fatal(err)
		}
		_, err = tx.ExecContext(ctx, `SELECT pg_sleep(10)`)
		classified(t, err, storage.ErrUnavailable, "57014")
	})

	t.Run("a statement whose caller's context ends", func(t *testing.T) {
		// The caller's own deadline: the store answers the context's error,
		// whatever the driver made of it.
		db := pgtest.Fresh(t)
		statementCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, err := db.ExecContext(statementCtx, `SELECT pg_sleep(10)`)
		if err == nil {
			t.Fatal("no error, want the statement cut short")
		}
		if got := classify(statementCtx, Postgres{}, err); got != context.DeadlineExceeded {
			t.Errorf("classify = %v, want context.DeadlineExceeded itself", got)
		}
	})

	t.Run("a lock timeout", func(t *testing.T) {
		db := pgtest.Fresh(t)
		if _, err := db.ExecContext(ctx, `CREATE TABLE counters (id int PRIMARY KEY, n int NOT NULL); INSERT INTO counters VALUES (1, 0)`); err != nil {
			t.Fatal(err)
		}
		holder, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback() }()
		if _, err := holder.ExecContext(ctx, `UPDATE counters SET n = n + 1 WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
		waiter, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = waiter.Rollback() }()
		if _, err := waiter.ExecContext(ctx, `SET LOCAL lock_timeout = '50ms'`); err != nil {
			t.Fatal(err)
		}
		_, err = waiter.ExecContext(ctx, `UPDATE counters SET n = n + 1 WHERE id = 1`)
		classified(t, err, storage.ErrUnavailable, "55P03")
	})

	t.Run("a connection the server ended", func(t *testing.T) {
		db := pgtest.Fresh(t)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		var pid int
		if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		// The second argument waits, up to 5 s, until the backend is gone.
		var ended bool
		if err := db.QueryRowContext(ctx, `SELECT pg_terminate_backend($1, 5000)`, pid).Scan(&ended); err != nil || !ended {
			t.Fatalf("end the transaction's connection: %v, %v", ended, err)
		}
		_, err = tx.ExecContext(ctx, `SELECT 1`)
		classified(t, err, storage.ErrUnavailable, "")
	})
}
