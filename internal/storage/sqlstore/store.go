// Package sqlstore keeps the control-plane state in a SQL database, through
// database/sql and a Dialect for what differs between engines: placeholders,
// error classification and the schema's migrations. Postgres is the first
// dialect. Its core — Open, the write gate, the transactions, the read
// routing, the error classification, the cursor and the migrator — is what the
// methods of the store run on.
//
// # Writes
//
// Every write runs in a write transaction: that of WithTx, or one of its own
// for a write method called outside WithTx. One write transaction runs at a
// time in a process, behind the store's gate, and waiting for the gate honours
// the context. A write transaction runs on the primary at READ COMMITTED, and
// the store's Fence checks it before anything else runs in it — before the
// callback of WithTx — so a replica that does not lead writes nothing; the
// store returns the fence's refusal unchanged. The callback runs once: nothing
// is retried. Its error, or a context done before the commit, rolls the
// transaction back. A context that ends while COMMIT is in flight can leave the
// transaction committed while the caller gets the context's error: the caller
// cannot tell, which is inherent, as with the apply timeout of raft. A
// transaction inside the callback is the same one, without a savepoint, so a
// nested callback's error rolls nothing back by itself.
//
// # Refused writes keep the transaction usable
//
// Postgres aborts a transaction at its first failed statement: every statement
// after it fails until the transaction rolls back. A write the store refuses
// inside a transaction must leave the transaction usable, as the in-memory
// store does: the user service goes on after a delete answers ErrNotFound, and
// commits. So the store refuses in Go, before any statement: the entity's
// validation; ErrNotFound from the rows a statement affected, never from a
// failed statement; insert-if-absent and ErrAlreadyExists through ON CONFLICT,
// never through a unique violation. A statement that can still fail runs
// inside a savepoint, rolled back to on failure.
//
// # Reads
//
// Point lookups, and every read of a transaction, read the primary. Listings,
// scans and counts read the replica, when the store has one, for a context
// marked storage.MarkReadOnly outside a transaction, and the primary
// otherwise: no decision rests on a lagging copy.
//
// # Errors
//
// The dialect classifies the driver's errors into the storage sentinels, and
// the error of a done context is returned as itself. No error the store
// returns, and no log line, carries a statement's arguments, a row's data, the
// engine's message or detail, or a connection string: an error names the
// engine's error code and the table, column and constraint at most.
//
// # Rows
//
// An entity's row is its ID, its creation time, the columns its lookups,
// criteria and counts read, and its body: the protojson of the entity's
// snapshot message (snapshot.Marshal…JSON), which carries every field. Every
// column that holds an entity's string is bytea, bound as []byte(s) and
// scanned into []byte: bytea compares and orders by bytes, as Go compares
// strings, with no collation, and carries a NUL, which text and jsonb refuse;
// a Go string bound to a bytea parameter would be read as bytea's text input
// form instead. A time a statement compares, created_at first, is a bigint of
// Unix nanoseconds, 0 for the zero time: it holds the years 1678 to 2262 only,
// and the store refuses a time outside them with storage.ErrInvalidArgument
// rather than wrap it.
package sqlstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// Fence keeps a replica that does not lead from writing.
type Fence interface {
	// Check runs inside each write transaction before its first write, and
	// refuses the transaction with an error the store returns unchanged, so it
	// must be one a caller may see. The transaction then rolls back.
	Check(ctx context.Context, tx *sql.Tx) error
}

// AllowAll is the Fence of a store that is the only writer of its database:
// it refuses nothing. Open uses it when Options names no fence.
type AllowAll struct{}

// Check refuses nothing.
func (AllowAll) Check(context.Context, *sql.Tx) error { return nil }

// Options are what Open builds a store from.
type Options struct {
	// Primary is the database every write and every decision reads.
	Primary *sql.DB
	// Replica, when set, serves the listings, scans and counts of contexts
	// marked storage.MarkReadOnly; nil: they read the primary.
	Replica *sql.DB
	// Dialect is the engine's.
	Dialect Dialect
	// Fence checks every write transaction; nil: AllowAll, for tests and a
	// store that is its database's only writer.
	Fence Fence
}

// Store keeps the control-plane state in a SQL database. The databases are the
// caller's: the store does not close them.
type Store struct {
	primary *sql.DB
	replica *sql.DB // nil: the listings read the primary
	dialect Dialect
	fence   Fence

	// gate holds one token while a write transaction runs: one at a time in
	// this process.
	gate chan struct{}
}

// Open returns a store on opts.Primary, whose schema must be the latest of the
// dialect's migrations: Open migrates nothing, and refuses a newer schema with
// ErrSchemaNewer and an older one, an empty database's included, with
// ErrSchemaOlder. It checks the primary alone: the replica is the primary's
// copy, which may lag a migration by a moment.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Primary == nil || opts.Dialect == nil {
		return nil, fmt.Errorf("%w: sqlstore: a store needs a primary database and a dialect", storage.ErrInvalidArgument)
	}
	files, err := migrationFiles(opts.Dialect.Migrations())
	if err != nil {
		return nil, err
	}
	version, err := SchemaVersion(ctx, opts.Primary)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(version, len(files)); err != nil {
		return nil, err
	}
	fence := opts.Fence
	if fence == nil {
		fence = AllowAll{}
	}
	return &Store{
		primary: opts.Primary,
		replica: opts.Replica,
		dialect: opts.Dialect,
		fence:   fence,
		gate:    make(chan struct{}, 1),
	}, nil
}
