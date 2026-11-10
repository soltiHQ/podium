package durable

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An open directory is refused with its mode named, and a symlink as one: an
// operator who reads the error knows which of the two to fix.
func TestInsecureAndSymlinkDirectoriesAreRejected(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o777} {
		dir := filepath.Join(t.TempDir(), "unsafe")
		if err := os.Mkdir(dir, mode); err != nil {
			t.Fatal(err)
		}
		// Ensure the intended insecure fixture mode even with a restrictive umask.
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		s, err := Open(dir)
		if err == nil {
			_ = s.Close()
			t.Fatalf("insecure directory %o accepted", mode)
		}
		if want := fmt.Sprintf("%s has mode %04o", dir, mode); !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "symlink") {
			t.Fatalf("Open(%o): %v, want it to say %q and not to name a symlink", mode, err, want)
		}
		if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
			t.Fatal("unsafe directory modified")
		}
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "private")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, link + string(os.PathSeparator)} {
		s, err := Open(path)
		if err == nil {
			_ = s.Close()
			t.Fatal("symlink directory accepted")
		}
		if want := link + " must be a directory, not a symlink"; !strings.Contains(err.Error(), want) {
			t.Fatalf("Open(%s): %v, want it to say %q", path, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(target, FileName)); !os.IsNotExist(err) {
		t.Fatal("symlink target modified")
	}
}

// A database file that is a symlink is refused as one, and its target is kept.
func TestSymlinkDatabaseIsRejectedWithoutChangingTarget(t *testing.T) {
	// t.TempDir is not private, which Open refuses before it looks at the file.
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	data := []byte("retain target")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err == nil {
		_ = s.Close()
		t.Fatal("symlink database accepted")
	}
	if want := "existing database is not a regular file"; !strings.Contains(err.Error(), want) {
		t.Fatalf("Open: %v, want the symlink refusal %q", err, want)
	}
	after, err := os.ReadFile(target)
	if err != nil || string(after) != string(data) {
		t.Fatal("symlink target changed")
	}
}
