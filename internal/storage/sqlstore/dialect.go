package sqlstore

import "io/fs"

// Dialect is what differs between the SQL engines the store runs on. The store
// writes its statements with ? placeholders and portable SQL, and asks the
// dialect for the rest. A second engine is a Dialect, its migrations, and a run
// of the storage contract suite.
type Dialect interface {
	// Name names the engine, as "postgres".
	Name() string

	// Rebind rewrites the ? placeholders of query into the engine's own, as
	// $1, $2… for Postgres. A ? in a string literal or a quoted identifier is
	// no placeholder.
	Rebind(query string) string

	// Classify maps an error of the engine's driver to the storage sentinels:
	// a unique violation to storage.ErrAlreadyExists; a lost or refused
	// connection, and a statement the engine could not run for now (a
	// transaction it gave up, a read-only server, a timeout, a lack of
	// resources), to storage.ErrUnavailable; anything else to
	// storage.ErrInternal. The error of a done context comes back as itself,
	// and an error that already wraps a storage sentinel unchanged. The error
	// it returns carries the engine's error code and the names of the table,
	// column and constraint at most: never a row's data, a statement's
	// arguments, the engine's message or detail, or a connection string.
	//
	// It cannot see the caller's context: a connection attempt that ended on the
	// caller's own deadline, a *pgconn.ConnectError that wraps it, answers
	// storage.ErrUnavailable. A caller outside the store checks its context
	// first, as the store's classify does, and returns the context's error.
	Classify(err error) error

	// Migrations holds the engine's migrations, NNNN_name.sql at its root,
	// versions 1, 2, 3… without a gap.
	Migrations() fs.FS
}
