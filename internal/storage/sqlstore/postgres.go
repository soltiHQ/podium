package sqlstore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/soltiHQ/control-plane/internal/storage"
)

//go:embed migrations/postgres/*.sql
var postgresFiles embed.FS

// postgresMigrations are the migrations of the Postgres dialect, at the root.
var postgresMigrations = func() fs.FS {
	sub, err := fs.Sub(postgresFiles, "migrations/postgres")
	if err != nil {
		// fs.Sub fails for an invalid path alone, and this one is a constant.
		panic(err)
	}
	return sub
}()

// Postgres is the Dialect of PostgreSQL, through pgx's database/sql driver
// (github.com/jackc/pgx/v5/stdlib, registered as "pgx").
type Postgres struct{}

// Name returns "postgres".
func (Postgres) Name() string { return "postgres" }

// Rebind numbers the ? placeholders of query $1, $2…, outside the string
// literals and quoted identifiers it skips: '…' and "…", a doubled quote
// within them included. It knows no escape string (E'…'), dollar quoting or
// comment, which the store's statements do not use.
func (Postgres) Rebind(query string) string {
	var (
		b     strings.Builder
		n     int
		quote byte // the quote of the literal or identifier being read; 0 outside
	)
	b.Grow(len(query) + 8)
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case quote != 0:
			// A doubled quote closes and opens again: the same outcome.
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// Classify maps an error of pgx or database/sql to the storage sentinels:
//
//   - SQLSTATE 23505, a unique violation: storage.ErrAlreadyExists;
//   - a statement the server could not run for now: SQLSTATE 40001 and 40P01,
//     a serialization failure and a deadlock; 25006, a write that reached a
//     read-only server, as a demoted primary; 57014, a statement timeout or a
//     cancel on the server's side; 55P03, a lock timeout; class 53, a lack of
//     resources: storage.ErrUnavailable;
//   - a connection that failed or was lost: a failed connection attempt
//     (*pgconn.ConnectError), SQLSTATE class 08, 57P01 to 57P03 and 57P05 (the
//     server ends the session or does not take it), a connection the driver or
//     database/sql gave up (driver.ErrBadConn, pgconn.ErrConnClosed), a
//     network error, a stream cut short: storage.ErrUnavailable;
//   - anything else, 57P04 (the database was dropped) included:
//     storage.ErrInternal.
//
// context.Canceled and context.DeadlineExceeded come back as themselves,
// unwrapped. A failed connection attempt is checked first, so that one that
// timed out (connect_timeout in the URL) is storage.ErrUnavailable, and so is
// 57014: a caller whose own context ended gets its context's error from the
// store, which checks the context before it classifies.
//
// The error returned names the SQLSTATE and the table, column and constraint a
// server error names, and nothing else of the error: PgError.Message and Detail
// quote rows and arguments, and a connection error names the user and the
// host.
func (Postgres) Classify(err error) error {
	var (
		connectErr *pgconn.ConnectError
		pgErr      *pgconn.PgError
	)
	switch {
	case err == nil:
		return nil
	case classified(err):
		return err
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.As(err, &connectErr):
		return fmt.Errorf("%w: postgres: the connection attempt failed", storage.ErrUnavailable)
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.As(err, &pgErr):
		return classifyPgError(pgErr)
	case errors.Is(err, pgconn.ErrConnClosed) || connectionLost(err):
		return fmt.Errorf("%w: postgres: the connection was lost", storage.ErrUnavailable)
	}
	return fmt.Errorf("%w: postgres: an error of the driver", storage.ErrInternal)
}

// classifyPgError maps an error the server sent by its SQLSTATE.
func classifyPgError(e *pgconn.PgError) error {
	sentinel := storage.ErrInternal
	switch e.Code {
	case "23505":
		sentinel = storage.ErrAlreadyExists
	case "40001", "40P01", "25006", "57014", "55P03", "57P01", "57P02", "57P03", "57P05":
		sentinel = storage.ErrUnavailable
	default:
		// Class 08 is a connection exception, class 53 a lack of resources.
		if strings.HasPrefix(e.Code, "08") || strings.HasPrefix(e.Code, "53") {
			sentinel = storage.ErrUnavailable
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "postgres: SQLSTATE %s", e.Code)
	for _, id := range [...]struct{ kind, name string }{
		{"table", e.TableName},
		{"column", e.ColumnName},
		{"constraint", e.ConstraintName},
	} {
		if id.name != "" {
			fmt.Fprintf(&b, ", %s %q", id.kind, id.name)
		}
	}
	return fmt.Errorf("%w: %s", sentinel, b.String())
}

// Migrations returns the migrations of the Postgres schema.
func (Postgres) Migrations() fs.FS { return postgresMigrations }
