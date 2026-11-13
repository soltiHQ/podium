package sqlstore

import (
	"bytes"
	"encoding/base64"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// TestCursorRoundTrip checks that a cursor is the place it was made at: the
// creation time to the nanosecond and the ID to the byte, a NUL and a byte that
// is not UTF-8 included; an empty cursor is the start of the listing.
func TestCursorRoundTrip(t *testing.T) {
	for _, place := range []cursor{
		{CreatedAt: 1, ID: []byte("a")},
		{CreatedAt: -1, ID: []byte("agent\x00a")},
		{CreatedAt: math.MaxInt64, ID: []byte{0xff, 0xfe}},
		{CreatedAt: math.MinInt64, ID: []byte("x")},
	} {
		got, err := decodeCursor(encodeCursor(place.CreatedAt, place.ID))
		if err != nil || got.CreatedAt != place.CreatedAt || !bytes.Equal(got.ID, place.ID) {
			t.Errorf("the cursor of (%d, %q) decodes to (%d, %q), %v", place.CreatedAt, place.ID, got.CreatedAt, got.ID, err)
		}
	}
	if got, err := decodeCursor(""); err != nil || got.CreatedAt != 0 || got.ID != nil {
		t.Errorf("an empty cursor decodes to (%d, %q), %v; want the start", got.CreatedAt, got.ID, err)
	}
}

// TestCursorForeignRefused checks that this store refuses a cursor the
// in-memory store made, and the in-memory store one this store made, and that
// this store refuses a malformed one: each with storage.ErrInvalidArgument.
func TestCursorForeignRefused(t *testing.T) {
	ctx := t.Context()
	mem := inmemory.New()
	for _, id := range []string{"agent-a", "agent-b"} {
		a, err := model.NewAgent(id, id, "https://"+id+".example.net")
		if err != nil {
			t.Fatal(err)
		}
		a.SetCreatedAt(time.Unix(100, 0))
		if err := mem.UpsertAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	page, err := mem.ListAgents(ctx, nil, storage.ListOptions{Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("an in-memory cursor: %v, %v", page, err)
	}
	if _, err := decodeCursor(page.NextCursor); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("decode the in-memory store's cursor: %v, want storage.ErrInvalidArgument", err)
	}
	ours := encodeCursor(time.Unix(100, 0).UnixNano(), []byte("agent-a"))
	if _, err := mem.ListAgents(ctx, nil, storage.ListOptions{Cursor: ours}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("the in-memory store with this store's cursor: %v, want storage.ErrInvalidArgument", err)
	}

	encoded := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct{ name, cursor string }{
		{"not base64url", "%%% not a cursor %%%"},
		{"not JSON", encoded("not JSON")},
		{"tagged for another backend", encoded(`{"b":"inmemory","v":1,"u":1,"i":"YQ=="}`)},
		{"of another version", encoded(`{"b":"sql","v":2,"u":1,"i":"YQ=="}`)},
		{"without an ID", encoded(`{"b":"sql","v":1,"u":1}`)},
		{"without a creation time", encoded(`{"b":"sql","v":1,"i":"YQ=="}`)},
		{"with an ID that is not base64", encoded(`{"b":"sql","v":1,"u":1,"i":"agent-a"}`)},
	} {
		if _, err := decodeCursor(tc.cursor); !errors.Is(err, storage.ErrInvalidArgument) {
			t.Errorf("decode a cursor %s: %v, want storage.ErrInvalidArgument", tc.name, err)
		}
	}
}
