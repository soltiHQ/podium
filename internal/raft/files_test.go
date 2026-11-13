package raft

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
)

// openEntries returns, with its mode, every entry under dir, dir itself
// included, that its group or others have any permission on.
func openEntries(t *testing.T, dir string) []string {
	t.Helper()
	var open []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o077 != 0 {
			open = append(open, fmt.Sprintf("%s %v", path, info.Mode()))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return open
}

// mkdirMode makes dir with exactly mode, whatever the umask.
func mkdirMode(t *testing.T, dir string, mode fs.FileMode) {
	t.Helper()
	if err := os.Mkdir(dir, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
}

// Nothing in a node's data directory is open to its group or others: neither
// the directory, snapshots/ and the BoltDB files from the first start, nor a
// snapshot's directory and files once the node takes one. A snapshot that an
// earlier version left 0755 and 0644 is made private at the next start.
func TestRaftFilesArePrivate(t *testing.T) {
	n := newLoopbackNode(t, newTestCA(t), "n1", reserveLoopbackAddrs(t, 1)[0])
	t.Cleanup(n.stop)
	n.start(t, n.self(), oneNodeTimeouts)
	waitFor(t, 2*time.Second, "n1 leads", n.profile.leadership.AmLeader)
	for _, name := range []string{"raft-log.bolt", "raft-stable.bolt", "snapshots"} {
		if _, err := os.Lstat(filepath.Join(n.dir, name)); err != nil {
			t.Fatalf("the first start made no %s: %v", name, err)
		}
	}
	if open := openEntries(t, n.dir); len(open) > 0 {
		t.Fatalf("open to the group or others after the first start: %v", open)
	}

	agent, err := model.NewAgent("snapshotted", "kept private", "http://agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.profile.Store().UpsertAgent(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if err := n.profile.raft.Snapshot().Error(); err != nil {
		t.Fatal(err)
	}
	states, err := filepath.Glob(filepath.Join(n.dir, "snapshots", "*", "state.bin"))
	if err != nil || len(states) == 0 {
		t.Fatalf("no snapshot on disk after a snapshot: %v, %v", states, err)
	}
	if open := openEntries(t, n.dir); len(open) > 0 {
		t.Fatalf("open to the group or others after a snapshot: %v", open)
	}

	n.stop()
	for _, state := range states {
		snapshot := filepath.Dir(state)
		if err := os.Chmod(snapshot, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"state.bin", "meta.json"} {
			if err := os.Chmod(filepath.Join(snapshot, name), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	n.start(t, n.self(), oneNodeTimeouts)
	if open := openEntries(t, n.dir); len(open) > 0 {
		t.Fatalf("open to the group or others after a restart from an earlier version's snapshot: %v", open)
	}
}

// A snapshot is private while it is written, not only once it is in place: a
// snapshot the node takes and one a leader installs on it are both written
// between the store's Create and the sink's Close.
func TestSnapshotsArePrivateWhileWritten(t *testing.T) {
	dir := privateTestDir(t)
	store, err := newSnapshotStore(dir, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	sink, err := store.Create(1, 10, 2, hraft.Configuration{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("state")); err != nil {
		t.Fatal(err)
	}
	if open := openEntries(t, dir); len(open) > 0 {
		_ = sink.Cancel()
		t.Fatalf("open to the group or others while written: %v", open)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, snapshotsDir, sink.ID(), "state.bin")); err != nil {
		t.Fatalf("the snapshot is not in place: %v", err)
	}
	if open := openEntries(t, dir); len(open) > 0 {
		t.Fatalf("open to the group or others once in place: %v", open)
	}
}

// entries returns the names of what dir holds.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list))
	for _, entry := range list {
		names = append(names, entry.Name())
	}
	return names
}

// A node refuses a data directory, or a snapshots directory in it, that its
// group or others can enter, or that is a symlink, written with a trailing
// slash or not: before it opens anything there, a
// log it holds already included, and without changing its mode.
func TestRaftRefusesLooseDataDir(t *testing.T) {
	ca := newTestCA(t)
	symlink := func(t *testing.T, path string) {
		if err := os.Symlink(privateTestDir(t), path); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, dir string) // makes the data directory New gets
		refused string                         // the directory refused, relative to the data directory
		slash   bool                           // whether New gets the data directory with a trailing slash
	}{
		{"data directory open to its group", func(t *testing.T, dir string) { mkdirMode(t, dir, 0o750) }, ".", false},
		{"data directory open to others", func(t *testing.T, dir string) { mkdirMode(t, dir, 0o701) }, ".", false},
		{"data directory is a symlink", symlink, ".", false},
		{"data directory is a symlink, written with a trailing slash", symlink, ".", true},
		{"snapshots directory open to its group", func(t *testing.T, dir string) {
			mkdirMode(t, dir, 0o700)
			mkdirMode(t, filepath.Join(dir, "snapshots"), 0o750)
		}, "snapshots", false},
		{"snapshots directory open to others", func(t *testing.T, dir string) {
			mkdirMode(t, dir, 0o700)
			mkdirMode(t, filepath.Join(dir, "snapshots"), 0o705)
		}, "snapshots", false},
		{"snapshots directory is a symlink", func(t *testing.T, dir string) {
			mkdirMode(t, dir, 0o700)
			symlink(t, filepath.Join(dir, "snapshots"))
		}, "snapshots", false},
		// The log holds no database, so a node that opened it would fail on that
		// instead.
		{"snapshots directory open to its group, next to a log", func(t *testing.T, dir string) {
			mkdirMode(t, dir, 0o700)
			if err := os.WriteFile(filepath.Join(dir, "raft-log.bolt"), []byte("not a database"), 0o600); err != nil {
				t.Fatal(err)
			}
			mkdirMode(t, filepath.Join(dir, "snapshots"), 0o750)
		}, "snapshots", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newLoopbackNode(t, ca, "n1", reserveLoopbackAddrs(t, 1)[0])
			t.Cleanup(n.stop)
			tc.prepare(t, n.dir)
			refused := filepath.Join(n.dir, tc.refused)
			before, err := os.Lstat(refused)
			if err != nil {
				t.Fatal(err)
			}
			held := entries(t, n.dir)
			err = n.open(n.self(), func(c *Config) {
				if tc.slash {
					c.DataDir += string(filepath.Separator)
				}
			})
			if !errors.Is(err, ErrDataDirNotPrivate) {
				t.Fatalf("New() = %v, want %v", err, ErrDataDirNotPrivate)
			}
			if now := entries(t, n.dir); !slices.Equal(now, held) {
				t.Fatalf("a refused data directory holds %v, want %v as it did", now, held)
			}
			after, err := os.Lstat(refused)
			if err != nil {
				t.Fatal(err)
			}
			if after.Mode() != before.Mode() {
				t.Fatalf("the refused directory has mode %v, want %v as it was", after.Mode(), before.Mode())
			}
		})
	}
}
