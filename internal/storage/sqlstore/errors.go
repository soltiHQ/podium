package sqlstore

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"

	"github.com/soltiHQ/control-plane/internal/storage"
)

var (
	// ErrSchemaNewer is the error of Open and Migrate on a database whose
	// schema a newer podium migrated: this podium neither reads nor writes it.
	// The error names both versions.
	ErrSchemaNewer = errors.New("sqlstore: the database schema is newer than this podium")

	// ErrSchemaOlder is the error of Open on a database whose schema is older
	// than this podium's, an empty one included: Migrate brings it forward.
	// The error names both versions.
	ErrSchemaOlder = errors.New("sqlstore: the database schema is older than this podium; run podium migrate")
)

// sentinels are the storage errors a classification answers.
var sentinels = [...]error{
	storage.ErrNotFound,
	storage.ErrAlreadyExists,
	storage.ErrConflict,
	storage.ErrInvalidArgument,
	storage.ErrUnavailable,
	storage.ErrInternal,
}

// classified reports whether err already wraps a storage sentinel, so that a
// classification leaves it as it is: classifying twice changes nothing.
func classified(err error) bool {
	for _, s := range sentinels {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// connectionLost reports whether err is a failure of the connection rather than
// of a statement, as database/sql and the network report one: a connection the
// driver gave up, a network error, or a stream that ended early. It is what any
// dialect's classification shares; a dialect adds its driver's own errors.
func connectionLost(err error) bool {
	var netErr net.Error
	return errors.Is(err, driver.ErrBadConn) ||
		errors.As(err, &netErr) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// classify returns what the store answers for err, an error of the driver: the
// error of ctx once ctx is done, whatever the driver made of it, and d's
// classification otherwise.
func classify(ctx context.Context, d Dialect, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return d.Classify(err)
}

// fail returns what a method of s answers for err, an error of the driver.
func (s *Store) fail(ctx context.Context, err error) error {
	return classify(ctx, s.dialect, err)
}
