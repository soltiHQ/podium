package durable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

// withHooks sets the seams a test stops or crashes the store at.
func withHooks(h hooks) Option { return func(o *options) { o.hooks = h } }

// writeSnapshot writes a snapshot now, due or not, as the snapshotter does.
func (s *Store) writeSnapshot(ctx context.Context) error {
	if err := s.writer.Acquire(ctx, 1); err != nil {
		return err
	}
	defer s.writer.Release(1)
	return s.snapshotLocked()
}

// failures records what a failure handler hears.
type failures struct {
	mu    sync.Mutex
	heard []error
}

// handler is the option that sets f's handler.
func (f *failures) handler() Option {
	return WithFailureHandler(func(err error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.heard = append(f.heard, err)
	})
}

func (f *failures) all() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.heard)
}

// heardOnce fails unless f heard exactly one failure, an ErrInternal whose text
// holds reason.
func (f *failures) heardOnce(t *testing.T, reason string) {
	t.Helper()
	if heard := f.all(); len(heard) != 1 || !errors.Is(heard[0], storage.ErrInternal) || !strings.Contains(heard[0].Error(), reason) {
		t.Fatalf("the failure handler heard %v, want one ErrInternal naming %q", heard, reason)
	}
}

// snapshotDue reports whether a snapshot is due, as a commit checks it.
func snapshotDue(s *Store) bool {
	_ = s.writer.Acquire(context.Background(), 1)
	defer s.writer.Release(1)
	return s.due()
}

// diskLayout is what a closed database file holds: its root buckets, and the meta,
// snapshot and log of the log layout when it has them.
type diskLayout struct {
	buckets       []string
	format        []byte
	snapshotIndex []byte
	blob          []byte
	log           []uint64
}

// readLayout reads the database file in dir, which no store may hold open.
func readLayout(t *testing.T, dir string) diskLayout {
	t.Helper()
	db, err := bolt.Open(filepath.Join(dir, FileName), 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var l diskLayout
	err = db.View(func(tx *bolt.Tx) error {
		if err := tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			l.buckets = append(l.buckets, string(name))
			return nil
		}); err != nil {
			return err
		}
		if b := tx.Bucket(metaBucket); b != nil {
			l.format = slices.Clone(b.Get(formatKey))
			l.snapshotIndex = slices.Clone(b.Get(snapshotIndexKey))
		}
		if b := tx.Bucket(snapshotBucket); b != nil {
			l.blob = slices.Clone(b.Get(blobKey))
		}
		if b := tx.Bucket(logBucket); b != nil {
			return b.ForEach(func(k, _ []byte) error {
				if len(k) != 8 {
					return fmt.Errorf("log key %x is not 8 bytes", k)
				}
				l.log = append(l.log, binary.BigEndian.Uint64(k))
				return nil
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// uint64Bytes is n as the 8 big-endian bytes the log layout stores numbers and log
// keys in.
func uint64Bytes(n uint64) []byte { return binary.BigEndian.AppendUint64(nil, n) }

// testEntry is the value of the log entry at index holding command: the command and
// the CRC-32C (Castagnoli) of the 8-byte index and the command, big-endian.
func testEntry(index uint64, command []byte) []byte {
	table := crc32.MakeTable(crc32.Castagnoli)
	sum := crc32.Update(crc32.Checksum(uint64Bytes(index), table), table, command)
	return binary.BigEndian.AppendUint32(slices.Clone(command), sum)
}

// upsertAgent writes one more agent in a transaction of its own: one log entry.
func upsertAgent(t *testing.T, s *Store, id string) {
	t.Helper()
	a, err := model.NewAgent(id, "agent-"+id, "http://"+id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAgent(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

// mutateFile applies change to the database file in dir in one bbolt transaction.
func mutateFile(t *testing.T, dir string, change func(*bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(filepath.Join(dir, FileName), 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Update(change); err != nil {
		t.Fatal(err)
	}
}

// rewriteBlob changes the snapshot that bucket holds under key, and its sha256.
func rewriteBlob(bucket, key []byte, change func(*raftv1.Snapshot)) func(*bolt.Tx) error {
	return func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		var msg raftv1.Snapshot
		if err := proto.Unmarshal(b.Get(key), &msg); err != nil {
			return err
		}
		change(&msg)
		data, err := proto.Marshal(&msg)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if err := b.Put(key, data); err != nil {
			return err
		}
		return b.Put(checksumKey, sum[:])
	}
}

// WithSnapshotThreshold sets the threshold; zero keeps the default and a negative
// one fails Open.
func TestSnapshotThresholdOption(t *testing.T) {
	for _, tc := range []struct {
		set, want int
	}{
		{0, DefaultSnapshotThreshold},
		{5, 5},
		{-1, 0},
	} {
		t.Run(fmt.Sprint(tc.set), func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "state"), WithSnapshotThreshold(tc.set))
			if tc.set < 0 {
				if !errors.Is(err, storage.ErrInvalidArgument) {
					t.Fatalf("Open with threshold %d: %v, want ErrInvalidArgument", tc.set, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if s.threshold != tc.want {
				t.Fatalf("threshold %d, want %d", s.threshold, tc.want)
			}
		})
	}
}

// A transaction that writes nothing commits nothing: bbolt neither begins a
// transaction nor writes a page, and the file keeps its bytes. One write then
// shows the counters move.
func TestReadOnlyTxDoesNoIO(t *testing.T) {
	s, dir := newTestStore(t)
	populate(t, s)
	ctx := context.Background()
	path := filepath.Join(dir, FileName)
	before := s.db.Stats()
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := agent.New(s, zerolog.Nop())
	for name, tx := range map[string]func() error{
		"reads": func() error {
			return s.WithTx(ctx, func(tx storage.Storage) error {
				if _, err := tx.GetAgent(ctx, "a1"); err != nil {
					return err
				}
				if _, err := tx.ListAgents(ctx, nil, storage.ListOptions{}); err != nil {
					return err
				}
				_, err := storage.AllRollouts(ctx, tx, nil)
				return err
			})
		},
		"bulk delete of nothing": func() error {
			return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteRolloutsBySpec(ctx, "missing") })
		},
		"heartbeat token check": func() error { return svc.VerifyOrEnrollToken(ctx, "a1", "test-enrollment-token") },
	} {
		if err := tx(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	after := s.db.Stats()
	diff := after.Sub(&before)
	if diff.TxN != 0 || diff.TxStats.GetWrite() != 0 {
		t.Fatalf("transactions that write nothing began %d bbolt read transactions and wrote %d pages, want none", diff.TxN, diff.TxStats.GetWrite())
	}
	if now, err := os.ReadFile(path); err != nil || !bytes.Equal(now, file) {
		t.Fatalf("transactions that write nothing changed the database file: %v", err)
	}
	upsertAgent(t, s, "a2")
	written := s.db.Stats()
	if written.TxStats.GetWrite() == after.TxStats.GetWrite() {
		t.Fatal("a write wrote no page: the counters show nothing")
	}
}

// A context canceled inside the bbolt transaction of a commit rolls it back: the
// caller gets context.Canceled, nothing is published, the store serves on, and the
// next write takes the index the canceled one left free.
func TestCancelInsideCommitChangesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		armed atomic.Bool
		heard failures
	)
	s, dir := newTestStore(t, heard.handler(), withHooks(hooks{commit: func() {
		if armed.Load() {
			cancel()
		}
	}}))
	populate(t, s)
	before := snapshotBytes(t, s)
	a, err := model.NewAgent("a2", "agent-a2", "http://a2")
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	if err := s.UpsertAgent(ctx, a); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel inside the commit: %v, want context.Canceled", err)
	}
	armed.Store(false)
	if !bytes.Equal(before, snapshotBytes(t, s)) {
		t.Fatal("a rolled back commit was published")
	}
	if _, err := s.GetAgent(context.Background(), "a2"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("read after the rollback: %v, want ErrNotFound", err)
	}
	if heard := heard.all(); len(heard) != 0 {
		t.Fatalf("a rolled back commit failed the store: %v", heard)
	}
	upsertAgent(t, s, "a3")
	assertReopen(t, s, dir, snapshotBytes(t, s))
	if l := readLayout(t, dir); !slices.Equal(l.log, []uint64{1, 2}) {
		t.Fatalf("log %v, want entries 1 and 2: the rolled back entry took no index", l.log)
	}
}

// The failure handler hears a failed commit or snapshot once, with the reason that
// later calls report, however many of them fail after it.
func TestFailureHandlerHearsEachFailureOnce(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name, reason string
		fail         func(*Store) error
	}{
		{"commit", "durable commit of log entry 2", func(s *Store) error {
			a, err := model.NewAgent("a2", "agent-a2", "http://a2")
			if err != nil {
				return err
			}
			return s.UpsertAgent(ctx, a)
		}},
		{"snapshot", "durable snapshot at log entry 1", func(s *Store) error { return s.writeSnapshot(ctx) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var heard failures
			s, _ := newTestStore(t, heard.handler())
			populate(t, s)
			// A closed bbolt handle fails the next bbolt transaction.
			if err := s.db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := test.fail(s); !errors.Is(err, storage.ErrInternal) {
				t.Fatalf("failed %s: %v, want ErrInternal", test.name, err)
			}
			if _, err := s.GetAgent(ctx, "a1"); !errors.Is(err, storage.ErrUnavailable) || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("read after a failed %s: %v, want ErrUnavailable naming %q", test.name, err, test.reason)
			}
			if err := s.DeleteAgent(ctx, "a1"); !errors.Is(err, storage.ErrUnavailable) {
				t.Fatalf("write after a failed %s: %v, want ErrUnavailable", test.name, err)
			}
			if err := s.writeSnapshot(ctx); !errors.Is(err, storage.ErrUnavailable) {
				t.Fatalf("snapshot after a failed %s: %v, want ErrUnavailable", test.name, err)
			}
			heard.heardOnce(t, test.reason)
		})
	}
}

// A snapshot whose bbolt transaction fails fails the store closed: reads and writes
// then report the failure.
func TestFailedSnapshotFailsClosed(t *testing.T) {
	s, _ := newTestStore(t)
	populate(t, s)
	ctx := context.Background()
	// A closed bbolt handle fails the snapshot's bbolt transaction.
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.writeSnapshot(ctx); !errors.Is(err, storage.ErrInternal) {
		t.Fatalf("failed snapshot: %v, want ErrInternal", err)
	}
	if _, err := s.GetAgent(ctx, "a1"); !errors.Is(err, storage.ErrUnavailable) || !strings.Contains(err.Error(), "durable snapshot at log entry 1") {
		t.Fatalf("read after a failed snapshot: %v, want ErrUnavailable naming the snapshot", err)
	}
	if err := s.WithTx(ctx, func(storage.Storage) error { return nil }); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("transaction after a failed snapshot: %v, want ErrUnavailable", err)
	}
}

// Below the threshold no snapshot is due, and none is written: with a threshold of
// 3, two entries stay in the log.
func TestNoSnapshotBelowThreshold(t *testing.T) {
	s, dir := newTestStore(t, WithSnapshotThreshold(3))
	populate(t, s)
	upsertAgent(t, s, "a2")
	if snapshotDue(s) {
		t.Fatal("a snapshot is due after 2 entries of a threshold of 3")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if l := readLayout(t, dir); !bytes.Equal(l.snapshotIndex, uint64Bytes(0)) || !slices.Equal(l.log, []uint64{1, 2}) {
		t.Fatalf("snapshot index %x, log %v, want no snapshot and entries 1 and 2", l.snapshotIndex, l.log)
	}
}

// A commit stalled inside its bbolt transaction, where it syncs, holds back
// neither a point read nor a scan, and neither sees the stalled write.
func TestReadsDoNotWaitForFsync(t *testing.T) {
	var (
		armed   atomic.Bool
		once    sync.Once
		entered = make(chan struct{})
		release = make(chan struct{})
	)
	s, _ := newTestStore(t, withHooks(hooks{commit: func() {
		if armed.Load() {
			once.Do(func() { close(entered) })
			<-release
		}
	}}))
	populate(t, s)
	ctx := context.Background()
	a, err := s.GetAgent(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	a.SetOS("darwin")
	armed.Store(true)
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	defer unblock()
	writer := make(chan error, 1)
	go func() { writer <- s.UpsertAgent(ctx, a) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the write never reached its commit")
	}

	type result struct {
		os  string
		err error
	}
	for _, read := range []struct {
		name string
		run  func() (string, error)
	}{
		{"GetAgent", func() (string, error) {
			got, err := s.GetAgent(ctx, "a1")
			if err != nil {
				return "", err
			}
			return got.OS(), nil
		}},
		{"ScanAgents", func() (string, error) {
			rows, err := s.ScanAgents(ctx, nil)
			if err != nil {
				return "", err
			}
			if len(rows) != 1 {
				return "", fmt.Errorf("%d agents, want 1", len(rows))
			}
			return rows[0].OS(), nil
		}},
	} {
		done := make(chan result, 1)
		go func() {
			os, err := read.run()
			done <- result{os, err}
		}()
		select {
		case r := <-done:
			if r.err != nil || r.os != "linux" {
				t.Fatalf("%s during the commit: OS %q, %v, want the committed linux", read.name, r.os, r.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s waited for the commit", read.name)
		}
	}
	unblock()
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetAgent(ctx, "a1"); err != nil || got.OS() != "darwin" {
		t.Fatalf("after the commit: %v, want darwin", err)
	}
}

// A snapshot stalled inside its bbolt transaction holds back transactions, which
// wait for the writer it holds, but not reads.
func TestReadsDoNotWaitForSnapshot(t *testing.T) {
	var (
		once    sync.Once
		entered = make(chan struct{})
		release = make(chan struct{})
	)
	s, _ := newTestStore(t, withHooks(hooks{snapshot: func(p snapshotPoint) {
		if p == snapshotWritten {
			once.Do(func() { close(entered) })
			<-release
		}
	}}))
	populate(t, s)
	ctx := context.Background()
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	defer unblock()
	snapshotted := make(chan error, 1)
	go func() { snapshotted <- s.writeSnapshot(ctx) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the snapshot never reached its write")
	}
	written := make(chan error, 1)
	go func() {
		a, err := model.NewAgent("a2", "agent-a2", "http://a2")
		if err != nil {
			written <- err
			return
		}
		written <- s.UpsertAgent(ctx, a)
	}()
	read := make(chan error, 1)
	go func() {
		if _, err := s.GetAgent(ctx, "a1"); err != nil {
			read <- err
			return
		}
		_, err := s.ScanAgents(ctx, nil)
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reads waited for the snapshot")
	}
	select {
	case err := <-written:
		t.Fatalf("a transaction finished during the snapshot: %v", err)
	default:
	}
	unblock()
	if err := <-snapshotted; err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

// A failed apply of a committed command fails the store closed and names the log
// entry. The entry is on disk, so a restart applies it.
func TestApplyFailureAfterCommitFailsClosed(t *testing.T) {
	var (
		armed atomic.Bool
		heard failures
		s     *Store
	)
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir, heard.handler(), withHooks(hooks{committed: func() {
		// Drift between the published state and what the transaction read: the
		// committed delete of a1 finds no a1 to delete.
		if armed.Load() {
			_ = s.inner.DeleteAgent(context.Background(), "a1")
		}
	}}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	populate(t, s)
	ctx := context.Background()
	armed.Store(true)
	err = s.DeleteAgent(ctx, "a1")
	if !errors.Is(err, storage.ErrInternal) || !strings.Contains(err.Error(), "log entry 2") {
		t.Fatalf("failed apply of a committed command: %v, want ErrInternal naming log entry 2", err)
	}
	if _, err := s.GetUser(ctx, "u1"); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("read after a failed apply: %v, want ErrUnavailable", err)
	}
	if err := s.WithTx(ctx, func(storage.Storage) error { return nil }); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("transaction after a failed apply: %v, want ErrUnavailable", err)
	}
	heard.heardOnce(t, "log entry 2")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	if _, err := restored.GetAgent(ctx, "a1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("committed delete after a restart: %v, want ErrNotFound", err)
	}
	if _, err := restored.GetUser(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
}

// An apply of a committed command that does not return, here because it panics,
// fails the store closed with a failure that names the log entry: a recovered
// panic leaves nothing half applied in sight.
func TestApplyPanicFailsClosed(t *testing.T) {
	var (
		armed atomic.Bool
		heard failures
	)
	s, _ := newTestStore(t, heard.handler(), withHooks(hooks{committed: func() {
		if armed.Load() {
			panic("the apply did not finish")
		}
	}}))
	populate(t, s)
	ctx := context.Background()
	armed.Store(true)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the apply did not panic")
			}
		}()
		_ = s.DeleteAgent(ctx, "a1")
	}()
	if _, err := s.GetAgent(ctx, "a1"); !errors.Is(err, storage.ErrUnavailable) || !strings.Contains(err.Error(), "log entry 2") {
		t.Fatalf("read after an apply that panicked: %v, want ErrUnavailable naming log entry 2", err)
	}
	heard.heardOnce(t, "log entry 2")
}

// A log entry that does not check out stops Open, which names it and leaves the
// file as it was. Entries 1 to 3 are committed; the cases with a snapshot took it
// at entry 3 and committed entry 4 after it.
func TestOpenRejectsCorruptLogEntry(t *testing.T) {
	refused, err := overlay.EncodeCommand([]*raftv1.Op{{Op: &raftv1.Op_AgentDelete{AgentDelete: "missing"}}})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := proto.Marshal(&raftv1.Command{Ops: []*raftv1.Op{{Op: &raftv1.Op_AgentDelete{AgentDelete: "a2"}}}})
	if err != nil {
		t.Fatal(err)
	}
	put := func(index uint64, value []byte) func(*bolt.Tx) error {
		return func(tx *bolt.Tx) error { return tx.Bucket(logBucket).Put(uint64Bytes(index), value) }
	}
	tests := []struct {
		name     string
		snapshot bool
		corrupt  func(*bolt.Tx) error
		want     string
	}{
		{"checksum", false, func(tx *bolt.Tx) error {
			b := tx.Bucket(logBucket)
			value := slices.Clone(b.Get(uint64Bytes(2)))
			value[len(overlay.CommandMagic)+2] ^= 0xff
			return b.Put(uint64Bytes(2), value)
		}, "log entry 2"},
		{"checksum of another index", false, func(tx *bolt.Tx) error {
			b := tx.Bucket(logBucket)
			return b.Put(uint64Bytes(2), slices.Clone(b.Get(uint64Bytes(3))))
		}, "log entry 2"},
		{"shorter than a checksum", false, put(2, []byte{1, 2, 3}), "log entry 2"},
		{"undecodable command", false, put(2, testEntry(2, []byte(overlay.CommandMagic+"\x03\xff\xff"))), "log entry 2"},
		{"headerless command", false, put(2, testEntry(2, legacy)), "log entry 2"},
		{"refused operation", false, put(2, testEntry(2, refused)), "log entry 2"},
		{"gap", false, func(tx *bolt.Tx) error { return tx.Bucket(logBucket).Delete(uint64Bytes(2)) }, "log entry 2"},
		{"first entry missing", false, func(tx *bolt.Tx) error { return tx.Bucket(logBucket).Delete(uint64Bytes(1)) }, "log entry 1"},
		{"entry 0", false, put(0, testEntry(0, refused)), "log entry 0"},
		{"key of another size", false, func(tx *bolt.Tx) error { return tx.Bucket(logBucket).Put([]byte("x"), []byte("y")) }, "log key"},
		{"bucket in the log", false, func(tx *bolt.Tx) error {
			_, err := tx.Bucket(logBucket).CreateBucket(uint64Bytes(4))
			return err
		}, "log entry 4"},
		{"entry at the snapshot index", true, func(tx *bolt.Tx) error {
			command, err := overlay.EncodeCommand([]*raftv1.Op{{Op: &raftv1.Op_AgentDelete{AgentDelete: "a3"}}})
			if err != nil {
				return err
			}
			return tx.Bucket(logBucket).Put(uint64Bytes(3), testEntry(3, command))
		}, "log entry 3"},
		{"snapshot index past the log", true, func(tx *bolt.Tx) error {
			return tx.Bucket(metaBucket).Put(snapshotIndexKey, uint64Bytes(4))
		}, "log entry 4"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, dir := newTestStore(t)
			populate(t, s)
			upsertAgent(t, s, "a2")
			upsertAgent(t, s, "a3")
			if test.snapshot {
				if err := s.writeSnapshot(context.Background()); err != nil {
					t.Fatal(err)
				}
				upsertAgent(t, s, "a4")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			mutateFile(t, dir, func(tx *bolt.Tx) error {
				if tx.Bucket(logBucket) == nil {
					return errors.New("the database has no log")
				}
				return test.corrupt(tx)
			})
			path := filepath.Join(dir, FileName)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := Open(dir)
			if err == nil {
				_ = restored.Close()
				t.Fatal("corrupt log accepted")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Open: %v, want an error naming %q", err, test.want)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("a refused Open changed the database file: %v", err)
			}
		})
	}
}

// A snapshot starts once the log holds the threshold of entries or 16 MiB, in the
// background after the commit that crosses either, and truncates the log.
func TestSnapshotTriggers(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		threshold int
		write     func(t *testing.T, s *Store)
	}{
		{"entries", 3, func(t *testing.T, s *Store) {
			populate(t, s)
			upsertAgent(t, s, "a2")
			upsertAgent(t, s, "a3")
		}},
		{"bytes", DefaultSnapshotThreshold, func(t *testing.T, s *Store) {
			// One transaction of 17 MiB: 544 records with an error of 32 KiB.
			message := strings.Repeat("e", 32<<10)
			if err := s.WithTx(ctx, func(tx storage.Storage) error {
				for i := range 544 {
					e, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "a1", TaskName: "task", TaskUID: fmt.Sprintf("uid-%d", i), Generation: 1, Attempt: 1, Status: "failed", Error: message, CapturedAt: time.Unix(2000, 0)})
					if err != nil {
						return err
					}
					if err := tx.UpsertExecution(ctx, e); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshotted := make(chan struct{}, 1)
			s, dir := newTestStore(t, WithSnapshotThreshold(test.threshold), withHooks(hooks{snapshot: func(p snapshotPoint) {
				if p == snapshotCommitted {
					select {
					case snapshotted <- struct{}{}:
					default:
					}
				}
			}}))
			test.write(t, s)
			select {
			case <-snapshotted:
			case <-time.After(10 * time.Second):
				t.Fatal("no snapshot after the commit that crossed the trigger")
			}
			want := snapshotBytes(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			l := readLayout(t, dir)
			if len(l.log) != 0 || bytes.Equal(l.snapshotIndex, uint64Bytes(0)) {
				t.Fatalf("after the snapshot: snapshot index %x, log %v, want a snapshot of every entry and no log", l.snapshotIndex, l.log)
			}
			if !bytes.Equal(l.blob, want) {
				t.Fatal("the snapshot is not the published state")
			}
			restored, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = restored.Close() }()
			if !bytes.Equal(snapshotBytes(t, restored), want) {
				t.Fatal("state differs after a restart from the snapshot")
			}
		})
	}
}

// Failure is nil while the store serves, and after a failed commit it is the reason
// that reads and writes report; a closed store has no failure.
func TestFailureNamesWhyTheStoreStopped(t *testing.T) {
	closed, _ := newTestStore(t)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closed.Failure(); err != nil {
		t.Fatalf("Failure of a closed store: %v, want none", err)
	}
	s, _ := newTestStore(t)
	populate(t, s)
	if err := s.Failure(); err != nil {
		t.Fatalf("Failure of a store that serves: %v", err)
	}
	// A closed bbolt handle fails the next bbolt transaction.
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := model.NewAgent("a2", "agent-a2", "http://a2")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertAgent(context.Background(), a); !errors.Is(err, storage.ErrInternal) {
		t.Fatalf("failed commit: %v, want ErrInternal", err)
	}
	if err = s.Failure(); !errors.Is(err, storage.ErrInternal) || !strings.Contains(err.Error(), "durable commit of log entry 2") {
		t.Fatalf("Failure after a failed commit: %v, want ErrInternal naming the commit", err)
	}
}
