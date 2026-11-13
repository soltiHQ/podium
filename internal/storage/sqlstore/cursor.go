package sqlstore

import (
	"encoding/base64"
	"encoding/json"

	"github.com/soltiHQ/control-plane/internal/storage"
)

const (
	cursorBackend = "sql"
	cursorVersion = 1
)

// cursor is a place in the listing order (created_at DESC, id ASC): the row it
// was made at, whose created_at and id the next page starts after. It is opaque
// to callers and self-describing for validation, so that each backend refuses
// another's cursor:
//   - b: the backend's tag, "sql";
//   - v: the version of the format, 1;
//   - u: the row's created_at, Unix nanoseconds;
//   - i: the row's id, its bytes, which JSON carries in base64.
type cursor struct {
	Backend   string `json:"b"`
	Version   int    `json:"v"`
	CreatedAt int64  `json:"u"`
	ID        []byte `json:"i"`
}

// encodeCursor returns the cursor of the row created at createdAt with id, as
// base64url of its JSON.
func encodeCursor(createdAt int64, id []byte) string {
	// A struct of a string, numbers and bytes always encodes.
	b, _ := json.Marshal(cursor{Backend: cursorBackend, Version: cursorVersion, CreatedAt: createdAt, ID: id})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor returns the place s names: the start of the listing, a cursor
// with no ID, for an empty s. It refuses with storage.ErrInvalidArgument a
// cursor that is not base64url of JSON, one of another backend or version, and
// one without an ID or a creation time: no row is created at 0, the Unix epoch,
// which the store's codecs refuse.
func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, storage.ErrInvalidArgument
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return cursor{}, storage.ErrInvalidArgument
	}
	if c.Backend != cursorBackend || c.Version != cursorVersion || len(c.ID) == 0 || c.CreatedAt == 0 {
		return cursor{}, storage.ErrInvalidArgument
	}
	return c, nil
}
