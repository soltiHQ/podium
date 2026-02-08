package inmemory

import (
	"encoding/base64"
	"encoding/json"

	"github.com/soltiHQ/control-plane/internal/storage"
)

const (
	cursorBackend = "inmemory"
	cursorVersion = 1
)

// cursor represents a position in the sorted entity list for pagination.
//
// It must align with the global ordering contract:
//
//	(UpdatedAt DESC, ID ASC)
//
// Cursor is intentionally opaque for callers, but self-describing for validation:
//   - b: backend tag (must match "inmemory")
//   - v: version (for forward-compatible changes)
type cursor struct {
	Backend string `json:"b"`
	Version int    `json:"v"`

	// UpdatedAtUnixNano stores UpdatedAt as unix nanoseconds.
	// Using int64 avoids RFC3339 parsing and timezone concerns.
	UpdatedAtUnixNano int64  `json:"u"`
	ID                string `json:"i"`
}

// encodeCursor serializes a cursor into an opaque base64 URL-safe string.
func encodeCursor(c cursor) (string, error) {
	c.Backend = cursorBackend
	c.Version = cursorVersion

	b, err := json.Marshal(c)
	if err != nil {
		return "", storage.ErrInternal
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeCursor deserializes a cursor from its string representation.
//
// Rules:
//   - Empty string is valid and represents the start of the list.
//   - Malformed base64 or JSON returns ErrInvalidArgument.
//   - Cursor must be produced by this backend (Backend == "inmemory").
//   - Cursor version must match (Version == 1).
//   - Missing ID or zero UpdatedAtUnixNano returns ErrInvalidArgument.
func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil
	}

	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, storage.ErrInvalidArgument
	}

	var c cursor
	if err = json.Unmarshal(b, &c); err != nil {
		return cursor{}, storage.ErrInvalidArgument
	}
	if c.Backend != cursorBackend || c.Version != cursorVersion {
		return cursor{}, storage.ErrInvalidArgument
	}
	if c.ID == "" || c.UpdatedAtUnixNano == 0 {
		return cursor{}, storage.ErrInvalidArgument
	}
	return c, nil
}
