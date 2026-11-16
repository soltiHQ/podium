package sqlstore

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// TestIndexKey checks the cap on the values bound to an indexed column: 2048
// bytes pass, one more is refused with storage.ErrInvalidArgument, without the
// value in the error, and an empty value is bound as an empty bytea, not NULL.
func TestIndexKey(t *testing.T) {
	long := strings.Repeat("k", maxKeyBytes+1)
	for _, tc := range []struct {
		name    string
		key     string
		refused bool
	}{
		{"empty", "", false},
		{"short", "agent-a", false},
		{"the largest", long[:maxKeyBytes], false},
		{"one byte over", long, true},
	} {
		b, err := indexKey("agent ID", tc.key)
		switch {
		case tc.refused && (b != nil || !errors.Is(err, storage.ErrInvalidArgument)):
			t.Errorf("%s: %d bytes, %v; want none and storage.ErrInvalidArgument", tc.name, len(b), err)
		case tc.refused && strings.Contains(err.Error(), "kkkk"):
			t.Errorf("%s: %q carries the key", tc.name, err)
		case !tc.refused && (err != nil || string(b) != tc.key || b == nil):
			t.Errorf("%s: %q, %v; want the key as non-nil bytes", tc.name, b, err)
		}
	}

	// The values of one composite index share one entry.
	if err := checkIndexEntry("credential", long[:maxKeyBytes-8], "password"); err != nil {
		t.Errorf("a pair that fills the entry: %v", err)
	}
	if err := checkIndexEntry("credential", long[:maxKeyBytes-7], "password"); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("a pair one byte over: %v, want storage.ErrInvalidArgument", err)
	}
}

// TestIndexKeyCapHoldsTheEntry checks, on Postgres, the cap's reason: a btree
// entry takes 2048 bytes that do not compress, and refuses far more with SQLSTATE
// 54000, which the store classifies as an internal error and which aborts its
// transaction.
func TestIndexKeyCapHoldsTheEntry(t *testing.T) {
	db := pgtest.Fresh(t)
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `CREATE TABLE keys (k bytea PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	insert := func(n int) error {
		k := make([]byte, n)
		_, _ = rand.Read(k)
		_, err := db.ExecContext(ctx, Postgres{}.Rebind(`INSERT INTO keys (k) VALUES (?)`), k)
		return err
	}
	if err := insert(maxKeyBytes); err != nil {
		t.Fatalf("a key of %d bytes: %v", maxKeyBytes, err)
	}
	err := insert(4 * maxKeyBytes)
	if err == nil {
		t.Fatalf("a key of %d bytes: no error, want the index to refuse it", 4*maxKeyBytes)
	}
	got := Postgres{}.Classify(err)
	if !errors.Is(got, storage.ErrInternal) || !strings.Contains(got.Error(), "54000") {
		t.Errorf("Classify = %v, want storage.ErrInternal naming SQLSTATE 54000", got)
	}
}
