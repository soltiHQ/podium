package inmemory

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestDecodeCursor_EmptyIsValid(t *testing.T) {
	t.Parallel()

	c, err := decodeCursor("")
	if err != nil {
		t.Fatalf("decodeCursor(empty) err=%v", err)
	}
	if c.ID != "" || c.UpdatedAtUnixNano != 0 {
		t.Fatalf("expected zero cursor, got=%+v", c)
	}
}

func TestDecodeCursor_MalformedBase64(t *testing.T) {
	t.Parallel()

	_, err := decodeCursor("!!!not_base64!!!")
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
}

func TestDecodeCursor_MalformedJSON(t *testing.T) {
	t.Parallel()

	raw := []byte("{not json")
	s := base64.RawURLEncoding.EncodeToString(raw)

	_, err := decodeCursor(s)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
}

func TestDecodeCursor_MissingFields(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"b": cursorBackend,
		"v": cursorVersion,
	}
	b, _ := json.Marshal(raw)
	s := base64.RawURLEncoding.EncodeToString(b)

	_, err := decodeCursor(s)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	raw = map[string]any{
		"b": cursorBackend,
		"v": cursorVersion,
		"i": "x",
	}
	b, _ = json.Marshal(raw)
	s = base64.RawURLEncoding.EncodeToString(b)

	_, err = decodeCursor(s)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	raw = map[string]any{
		"b": cursorBackend,
		"v": cursorVersion,
		"u": time.Unix(10, 0).UTC().UnixNano(),
	}
	b, _ = json.Marshal(raw)
	s = base64.RawURLEncoding.EncodeToString(b)

	_, err = decodeCursor(s)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
}

func TestDecodeCursor_ForeignBackend(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"b": "other-backend",
		"v": cursorVersion,
		"u": time.Unix(10, 0).UTC().UnixNano(),
		"i": "x",
	}
	b, _ := json.Marshal(raw)
	s := base64.RawURLEncoding.EncodeToString(b)

	_, err := decodeCursor(s)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
}

func TestDecodeCursor_UnsupportedVersion(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"b": cursorBackend,
		"v": cursorVersion + 999,
		"u": time.Unix(10, 0).UTC().UnixNano(),
		"i": "x",
	}
	b, _ := json.Marshal(raw)
	s := base64.RawURLEncoding.EncodeToString(b)

	_, err := decodeCursor(s)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
}

func TestEncodeCursor_RoundTripAndForcesBackendVersion(t *testing.T) {
	t.Parallel()

	wantU := time.Unix(123, 0).UTC().UnixNano()

	s, err := encodeCursor(cursor{
		Backend:           "evil",
		Version:           999,
		UpdatedAtUnixNano: wantU,
		ID:                "id-1",
	})
	if err != nil {
		t.Fatalf("encodeCursor err=%v", err)
	}
	if s == "" {
		t.Fatalf("expected non-empty cursor string")
	}

	c, err := decodeCursor(s)
	if err != nil {
		t.Fatalf("decodeCursor(roundtrip) err=%v", err)
	}

	if c.Backend != cursorBackend {
		t.Fatalf("expected backend=%q got=%q", cursorBackend, c.Backend)
	}
	if c.Version != cursorVersion {
		t.Fatalf("expected version=%d got=%d", cursorVersion, c.Version)
	}
	if c.ID != "id-1" {
		t.Fatalf("expected id=%q got=%q", "id-1", c.ID)
	}
	if c.UpdatedAtUnixNano != wantU {
		t.Fatalf("expected u=%d got=%d", wantU, c.UpdatedAtUnixNano)
	}
}

func TestEncodeCursor_ProducesDecodableToken(t *testing.T) {
	t.Parallel()

	u := time.Unix(1, 0).UTC().UnixNano()

	s, err := encodeCursor(cursor{
		UpdatedAtUnixNano: u,
		ID:                "x",
	})
	if err != nil {
		t.Fatalf("encodeCursor err=%v", err)
	}
	if s == "" {
		t.Fatalf("expected non-empty token")
	}

	c, err := decodeCursor(s)
	if err != nil {
		t.Fatalf("decodeCursor err=%v", err)
	}
	if c.ID != "x" || c.UpdatedAtUnixNano != u {
		t.Fatalf("unexpected cursor: %+v", c)
	}
}
