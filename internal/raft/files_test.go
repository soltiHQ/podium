package raft

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

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
	store, err := newSnapshotStore(dir)
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

// A node refuses a data directory, or a snapshots directory in it, that its
// group or others can enter, or that is a symlink, as the durable store does:
// before it opens anything there, and without changing its mode.
func TestRaftRefusesLooseDataDir(t *testing.T) {
	ca := newTestCA(t)
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, dir string) // makes the data directory New gets
		refused string                         // the directory refused, relative to the data directory
	}{
		{"data directory open to its group", func(t *testing.T, dir string) { mkdirMode(t, dir, 0o750) }, "."},
		{"data directory open to others", func(t *testing.T, dir string) { mkdirMode(t, dir, 0o701) }, "."},
		{"data directory is a symlink", func(t *testing.T, dir string) {
			if err := os.Symlink(privateTestDir(t), dir); err != nil {
				t.Fatal(err)
			}
		}, "."},
		{"snapshots directory open to its group", func(t *testing.T, dir string) {
			mkdirMode(t, dir, 0o700)
			mkdirMode(t, filepath.Join(dir, "snapshots"), 0o750)
		}, "snapshots"},
		{"snapshots directory open to others", func(t *testing.T, dir string) {
			mkdirMode(t, dir, 0o700)
			mkdirMode(t, filepath.Join(dir, "snapshots"), 0o705)
		}, "snapshots"},
		{"snapshots directory is a symlink", func(t *testing.T, dir string) {
			mkdirMode(t, dir, 0o700)
			if err := os.Symlink(privateTestDir(t), filepath.Join(dir, "snapshots")); err != nil {
				t.Fatal(err)
			}
		}, "snapshots"},
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
			if err := n.open(n.self()); !errors.Is(err, ErrDataDirNotPrivate) {
				t.Fatalf("New() = %v, want %v", err, ErrDataDirNotPrivate)
			}
			for _, name := range []string{"raft-log.bolt", "raft-stable.bolt"} {
				if _, err := os.Lstat(filepath.Join(n.dir, name)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("a refused data directory holds %s: %v", name, err)
				}
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
