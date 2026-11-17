package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"maps"
	"slices"
	"testing"
)

// checksums are the SHA-256 sums of the migration files. A migration in a
// release never changes: a database it migrated holds its version and never
// runs it again, and Bytebase, which keeps the sum of each file it applied,
// flags a changed one. So a change to a file shows here, as a change of its
// sum, which only a migration in no release yet may make; a new migration adds
// its line, and none is removed.
var checksums = map[string]string{
	"0001_init.sql":             "bb345de73acd39e80b58a3cd78fc54d925a43b5296ba8e517d52b1fd4d0d4de4",
	"0002_workload_schemas.sql": "792cc7774bb9f8a767985c9c6f2a659b920a944b825b0056251be6b4cafc8a56",
}

// migrationNames returns the names of the Postgres migrations, in order.
func migrationNames(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(Postgres(), ".")
	if err != nil || len(entries) == 0 {
		t.Fatalf("the Postgres migrations: %v, %v", entries, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestMigrationsKeepTheContract runs the checks of breaks on every migration.
// A migration that must remove or change what the podium before it uses, once
// no such podium runs, changes this test with its reason.
func TestMigrationsKeepTheContract(t *testing.T) {
	for _, name := range migrationNames(t) {
		body, err := fs.ReadFile(Postgres(), name)
		if err != nil {
			t.Fatal(err)
		}
		stmts, err := statements(string(body))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, b := range breaks(stmts) {
			t.Errorf("%s: %s", name, b)
		}
	}
}

// TestMigrationsAreFrozen compares every migration with its sum in checksums.
func TestMigrationsAreFrozen(t *testing.T) {
	names := migrationNames(t)
	for _, name := range names {
		body, err := fs.ReadFile(Postgres(), name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		switch want, ok := checksums[name]; {
		case !ok:
			t.Errorf("%s has no checksum: a new migration adds its line to checksums, %q", name, got)
		case got != want:
			t.Errorf("%s changed: its sum is %s, want %s; a migration in a release never changes, a new one changes the schema", name, got, want)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(checksums)) {
		if !slices.Contains(names, name) {
			t.Errorf("%s is gone: a migration is never removed", name)
		}
	}
}
