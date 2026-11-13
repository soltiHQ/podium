package sqlstore

import (
	"fmt"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// maxKeyBytes is the most the store lets a value bound to an indexed column
// hold: an ID, a foreign ID, a subject, a role name. A btree entry is capped
// near 2.7 kB, and a statement that writes a larger one fails with SQLSTATE
// 54000, which aborts the transaction it runs in; so the store refuses such a
// value in Go, before any statement. Memory and raft accept longer keys.
const maxKeyBytes = 2048

// bin returns s as the bytes a bytea parameter takes. The slice is never nil,
// so that an empty string binds as an empty bytea, not as NULL.
func bin(s string) []byte { return append([]byte{}, s...) }

// indexKey returns s, the what of an entity, as the bytes a bytea parameter
// takes, refusing it with storage.ErrInvalidArgument when it is too long for an
// index entry. Every write of an entity binds its indexed values through it. The
// error names the value and its length, never its content.
func indexKey(what, s string) ([]byte, error) {
	if err := checkIndexEntry(what, s); err != nil {
		return nil, err
	}
	return bin(s), nil
}

// checkIndexEntry refuses with storage.ErrInvalidArgument values, the what of an
// entity, that together are too long for one index entry: the values of the
// columns of one index, which a composite index holds in one entry.
func checkIndexEntry(what string, values ...string) error {
	var n int
	for _, v := range values {
		n += len(v)
	}
	if n > maxKeyBytes {
		return fmt.Errorf("%w: sqlstore: the %s is %d bytes, over the %d an index entry holds", storage.ErrInvalidArgument, what, n, maxKeyBytes)
	}
	return nil
}
