package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
	"github.com/soltiHQ/control-plane/migrations"
)

// "podium schema version" prints the version of podium's newest migration, the
// schema version this podium runs on, and starts nothing.
func TestSchemaVersionCommand(t *testing.T) {
	latest, err := sqlstore.LatestVersion(sqlstore.Postgres{})
	if err != nil || latest < 1 {
		t.Fatalf("LatestVersion: %d, %v", latest, err)
	}
	var spy serveSpy
	var stdout, stderr bytes.Buffer
	if code := run([]string{"schema", "version"}, &stdout, &stderr, spy.serve); code != 0 || spy.started || stderr.Len() != 0 || stdout.String() != fmt.Sprintf("%d\n", latest) {
		t.Fatalf("exit %d, started %v, stdout %q, stderr %q; want exit 0 and %d", code, spy.started, stdout.String(), stderr.String(), latest)
	}
}

// exported returns the migration files, by name, with their contents.
func exported(t *testing.T) map[string][]byte {
	t.Helper()
	entries, err := fs.ReadDir(migrations.Postgres(), ".")
	if err != nil || len(entries) == 0 {
		t.Fatalf("the migrations: %v, %v", entries, err)
	}
	files := map[string][]byte{}
	for _, e := range entries {
		body, err := fs.ReadFile(migrations.Postgres(), e.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = body
	}
	return files
}

// dirFiles returns the files in dir, by name, with their contents.
func dirFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = body
	}
	return files
}

// sameFiles reports whether a and b hold the same files with the same contents.
func sameFiles(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for name, body := range a {
		if other, ok := b[name]; !ok || !bytes.Equal(body, other) {
			return false
		}
	}
	return true
}

// "podium schema export --dir DIR" writes podium's migrations into DIR, which it
// creates, each file as podium applies it, and names each file it wrote; run
// again, it keeps them, and names each file it kept. A file of DIR that the
// migrations do not hold stays. A file of a migration's name with other content
// stops the export before it writes any file, exit 1, naming the file, which
// stays as it was; so does a DIR it cannot make.
func TestSchemaExportCommand(t *testing.T) {
	want := exported(t)
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range want {
			if !yield(name) {
				return
			}
		}
	})
	dir := filepath.Join(t.TempDir(), "podium")
	var spy serveSpy
	var stdout, stderr bytes.Buffer
	if code := run([]string{"schema", "export", "--dir", dir}, &stdout, &stderr, spy.serve); code != 0 || spy.started || stderr.Len() != 0 {
		t.Fatalf("export: exit %d, started %v, stderr %q; want exit 0", code, spy.started, stderr.String())
	}
	if got := dirFiles(t, dir); !sameFiles(got, want) {
		t.Fatalf("the files exported: %q, want the migrations %q", slices.Collect(func(yield func(string) bool) {
			for name := range got {
				if !yield(name) {
					return
				}
			}
		}), names)
	}
	for _, name := range names {
		if line := "wrote " + filepath.Join(dir, name) + "\n"; !strings.Contains(stdout.String(), line) {
			t.Errorf("stdout %q lacks %q", stdout.String(), line)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("ours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := run([]string{"schema", "export", "--dir=" + dir}, &stdout, &stderr, spy.serve); code != 0 || stderr.Len() != 0 {
		t.Fatalf("the export again: exit %d, stderr %q; want exit 0", code, stderr.String())
	}
	for _, name := range names {
		if line := "kept " + filepath.Join(dir, name) + "\n"; !strings.Contains(stdout.String(), line) {
			t.Errorf("stdout %q lacks %q", stdout.String(), line)
		}
	}
	if got := dirFiles(t, dir); len(got) != len(want)+1 || string(got["README.md"]) != "ours\n" {
		t.Errorf("the files after the export again: %d, README.md %q; want the migrations and README.md as it was", len(got), got["README.md"])
	}

	// A file of the first migration's name with other content, in a fresh DIR.
	conflict := filepath.Join(t.TempDir(), "podium")
	if err := os.Mkdir(conflict, 0o755); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(conflict, names[0])
	if err := os.WriteFile(edited, []byte("-- edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code := run([]string{"schema", "export", "--dir", conflict}, &stdout, &stderr, spy.serve)
	if code != 1 || !strings.Contains(stderr.String(), edited) || stdout.Len() != 0 {
		t.Fatalf("an export onto an edited file: exit %d, stdout %q, stderr %q; want exit 1 naming %s and nothing written", code, stdout.String(), stderr.String(), edited)
	}
	if got := dirFiles(t, conflict); len(got) != 1 || string(got[names[0]]) != "-- edited\n" {
		t.Errorf("the files after the refusal: %d, %s %q; want the edited file alone, as it was", len(got), names[0], got[names[0]])
	}

	// A DIR it cannot make: a file stands at its path.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := run([]string{"schema", "export", "--dir", filepath.Join(blocked, "podium")}, &stdout, &stderr, spy.serve); code != 1 || !strings.Contains(stderr.String(), "schema export") {
		t.Fatalf("an export into a DIR it cannot make: exit %d, stderr %q; want exit 1", code, stderr.String())
	}
}

// podium schema takes version, or export with --dir DIR, alone: anything else
// is a usage error of its own, exit 2, that writes nothing.
func TestSchemaCommandLine(t *testing.T) {
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"schema"}, "schema: version or export"},
		{[]string{"schema", "bogus"}, `schema: unknown subcommand "bogus"`},
		{[]string{"schema", "version", "extra"}, "schema version takes no arguments"},
		{[]string{"schema", "export"}, "schema export: --dir is required"},
		{[]string{"schema", "export", "--dir"}, "schema export: flag needs an argument: -dir"},
		{[]string{"schema", "export", "--dir", "x", "extra"}, `schema export: unexpected argument "extra"`},
		{[]string{"schema", "export", "--bogus"}, "schema export: flag provided but not defined: -bogus"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var spy serveSpy
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr, spy.serve)
			if code != 2 || spy.started || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.says) || !strings.Contains(stderr.String(), "usage: podium") {
				t.Fatalf("exit %d, started %v, stdout %q, stderr %q; want exit 2 saying %q, with the usage", code, spy.started, stdout.String(), stderr.String(), tc.says)
			}
		})
	}
	if _, err := os.Stat("x"); err == nil {
		t.Errorf("a usage error made the directory x")
	}
}
