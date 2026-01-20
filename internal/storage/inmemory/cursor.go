package inmemory

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// cursor represents a position in the sorted entity list for pagination.
type cursor struct {
	UpdatedAt time.Time `json:"u"`
	ID        string    `json:"i"`
}

// encodeCursor serializes a cursor into an opaque base64-encoded string.
func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.URLEncoding.EncodeToString(b)
}

// decodeCursor deserializes a cursor from its string representation.
//
// Returns storage.ErrInvalidArgument for malformed cursors or cursors missing required fields.
// Empty string cursors are treated as valid (start of a list).
func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil
	}
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, storage.ErrInvalidArgument
	}

	var c cursor
	if err = json.Unmarshal(b, &c); err != nil {
		return cursor{}, storage.ErrInvalidArgument
	}
	if c.ID == "" {
		return cursor{}, storage.ErrInvalidArgument
	}
	return c, nil
}
