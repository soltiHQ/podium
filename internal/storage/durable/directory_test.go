package durable

import (
	"os"
	"path/filepath"
	"testing"
)

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
		if s, err := Open(dir); err == nil {
			_ = s.Close()
			t.Fatalf("insecure directory %o accepted", mode)
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
		if s, err := Open(path); err == nil {
			_ = s.Close()
			t.Fatal("symlink directory accepted")
		}
	}
	if _, err := os.Stat(filepath.Join(target, FileName)); !os.IsNotExist(err) {
		t.Fatal("symlink target modified")
	}
}

func TestSymlinkDatabaseIsRejectedWithoutChangingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	data := []byte("retain target")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(dir); err == nil {
		_ = s.Close()
		t.Fatal("symlink database accepted")
	}
	after, err := os.ReadFile(target)
	if err != nil || string(after) != string(data) {
		t.Fatal("symlink target changed")
	}
}
