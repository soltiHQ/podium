package durable

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// The crash tests run their writes in a child process that exits at a seam with
// os.Exit, which skips Close and all deferred work, as an abrupt exit does. The
// parent then reads the file and reopens it. Neither models power loss.

// runChild runs the test named test in a child process with env added to its
// environment, and fails unless the child exits 0.
func runChild(t *testing.T, test string, env ...string) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	child.Env = append(os.Environ(), env...)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

// createSessionDeleteAgent creates session s2 and deletes agent a1, in one
// transaction. Applied a second time, both operations fail.
func createSessionDeleteAgent(ctx context.Context, s storage.Storage) error {
	return s.WithTx(ctx, func(tx storage.Storage) error {
		session, err := model.NewSession("s2", "u1", "c1", enum.Password, []byte("hash-2"), time.Unix(4_000_000_000, 0))
		if err != nil {
			return err
		}
		if err := tx.CreateSession(ctx, session); err != nil {
			return err
		}
		return tx.DeleteAgent(ctx, "a1")
	})
}

// A process that exits after a command commits and before the store applies it
// leaves the command in the log, and a restart applies it once: the transaction
// creates a session and deletes an agent, which fail when applied twice.
func TestWALReplaysCommittedCommandOnce(t *testing.T) {
	const childEnv = "SOLTI_DURABLE_WAL_CHILD_DIR"
	ctx := context.Background()
	if dir := os.Getenv(childEnv); dir != "" {
		var armed atomic.Bool
		s, err := Open(dir, withHooks(hooks{committed: func() {
			if armed.Load() {
				os.Exit(0)
			}
		}}))
		if err != nil {
			t.Fatal(err)
		}
		populate(t, s)
		armed.Store(true)
		err = createSessionDeleteAgent(ctx, s)
		t.Fatalf("the transaction returned %v instead of exiting between its commit and its apply", err)
	}
	dir := filepath.Join(t.TempDir(), "crash-state")
	runChild(t, "TestWALReplaysCommittedCommandOnce", childEnv+"="+dir)
	if l := readLayout(t, dir); !bytes.Equal(l.snapshotIndex, uint64Bytes(0)) || !slices.Equal(l.log, []uint64{1, 2}) {
		t.Fatalf("after the exit: snapshot index %x, log %v, want no snapshot and entries 1 and 2", l.snapshotIndex, l.log)
	}

	restored, err := Open(dir)
	if err != nil {
		t.Fatalf("restart after a committed, unapplied command: %v", err)
	}
	sessions, err := restored.ListSessionsByUser(ctx, "u1")
	if err != nil || len(sessions) != 2 {
		_ = restored.Close()
		t.Fatalf("sessions of u1: %d, %v, want s1 and the replayed s2", len(sessions), err)
	}
	if _, err := restored.GetAgent(ctx, "a1"); !errors.Is(err, storage.ErrNotFound) {
		_ = restored.Close()
		t.Fatalf("agent a1 after the replayed delete: %v, want ErrNotFound", err)
	}
	upsertAgent(t, restored, "a2")
	want := snapshotBytes(t, restored)
	assertReopen(t, restored, dir, want)
	if l := readLayout(t, dir); !slices.Equal(l.log, []uint64{1, 2, 3}) {
		t.Fatalf("log %v, want the write after the restart at entry 3", l.log)
	}
}

// A snapshot writes the state at the last entry and deletes the entries up to it in
// one bbolt transaction. A process that exits inside that transaction, after the
// snapshot write or after the truncation, leaves the previous snapshot and the
// whole log; one that exits after its commit leaves the new snapshot and no entry
// up to it. Every case reopens to the state the child acknowledged.
func TestSnapshotTruncatesLog(t *testing.T) {
	const pointEnv, dirEnv = "SOLTI_DURABLE_SNAPSHOT_CRASH_POINT", "SOLTI_DURABLE_SNAPSHOT_CRASH_DIR"
	points := map[string]snapshotPoint{"written": snapshotWritten, "truncated": logTruncated, "committed": snapshotCommitted}
	ctx := context.Background()
	if dir := os.Getenv(dirEnv); dir != "" {
		point, ok := points[os.Getenv(pointEnv)]
		if !ok {
			t.Fatalf("unknown crash point %q", os.Getenv(pointEnv))
		}
		s, err := Open(dir, withHooks(hooks{snapshot: func(p snapshotPoint) {
			if p == point {
				os.Exit(0)
			}
		}}))
		if err != nil {
			t.Fatal(err)
		}
		populate(t, s)
		upsertAgent(t, s, "a2")
		if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "acknowledged"), snapshotBytes(t, s), 0o600); err != nil {
			t.Fatal(err)
		}
		err = s.writeSnapshot(ctx)
		t.Fatalf("the snapshot returned %v instead of exiting at its crash point", err)
	}

	t.Run("no crash", func(t *testing.T) {
		s, dir := newTestStore(t)
		populate(t, s)
		upsertAgent(t, s, "a2")
		atTwo := snapshotBytes(t, s)
		if err := s.writeSnapshot(ctx); err != nil {
			t.Fatal(err)
		}
		upsertAgent(t, s, "a3")
		want := snapshotBytes(t, s)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if l := readLayout(t, dir); !bytes.Equal(l.snapshotIndex, uint64Bytes(2)) || !slices.Equal(l.log, []uint64{3}) || !bytes.Equal(l.blob, atTwo) {
			t.Fatalf("snapshot index %x, log %v, want the state at entry 2 and entry 3 alone", l.snapshotIndex, l.log)
		}
		restored, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = restored.Close() }()
		if !bytes.Equal(snapshotBytes(t, restored), want) {
			t.Fatal("state differs after a restart from the snapshot and the log")
		}
	})
	for _, name := range []string{"written", "truncated", "committed"} {
		t.Run("exit after "+name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			runChild(t, "TestSnapshotTruncatesLog", pointEnv+"="+name, dirEnv+"="+dir)
			acknowledged, err := os.ReadFile(filepath.Join(filepath.Dir(dir), "acknowledged"))
			if err != nil {
				t.Fatal(err)
			}
			l := readLayout(t, dir)
			if points[name] == snapshotCommitted {
				if !bytes.Equal(l.snapshotIndex, uint64Bytes(2)) || len(l.log) != 0 || !bytes.Equal(l.blob, acknowledged) {
					t.Fatalf("snapshot index %x, log %v, want the snapshot at entry 2 and no entry", l.snapshotIndex, l.log)
				}
			} else if !bytes.Equal(l.snapshotIndex, uint64Bytes(0)) || !slices.Equal(l.log, []uint64{1, 2}) {
				t.Fatalf("snapshot index %x, log %v, want no snapshot and entries 1 and 2", l.snapshotIndex, l.log)
			}
			restored, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = restored.Close() }()
			if !bytes.Equal(snapshotBytes(t, restored), acknowledged) {
				t.Fatal("state differs from what the child acknowledged")
			}
		})
	}
}

// A process that exits inside the migration leaves the single-blob file as it was,
// and the next Open migrates it.
func TestExitDuringMigrationKeepsSingleBlob(t *testing.T) {
	const childEnv = "SOLTI_DURABLE_MIGRATION_CHILD_DIR"
	if dir := os.Getenv(childEnv); dir != "" {
		s, err := Open(dir, withHooks(hooks{migrate: func() { os.Exit(0) }}))
		if err == nil {
			_ = s.Close()
		}
		t.Fatalf("Open returned %v instead of exiting in the migration", err)
	}
	dir := singleBlobStore(t)
	before, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	runChild(t, "TestExitDuringMigrationKeepsSingleBlob", childEnv+"="+dir)
	if after, err := os.ReadFile(filepath.Join(dir, FileName)); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("an exit inside the migration changed the file: %v", err)
	}
	want := atCurrentVersion(t, singleBlob(t, dir))
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if !bytes.Equal(snapshotBytes(t, s), want) {
		t.Fatal("the migrated state differs from the single-blob snapshot")
	}
}
