package durable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"slices"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
	bolt "go.etcd.io/bbolt"
)

// The log layout, format 2, keeps three buckets:
//
//   - meta: format, 2, and snapshot_index, the last log entry the snapshot holds;
//   - snapshot: blob, the SOLTI-SNAP state at snapshot_index, and its sha256;
//   - log: the entries past snapshot_index, by index. An entry is a SOLTI-CMD
//     command followed by the CRC-32C of the index and the command.
//
// Numbers and keys are 8 bytes, big-endian, so the log iterates in index order.
// The single-blob layout before it, format 1, kept the whole state in one
// snapshot of the control-plane-state bucket.
var (
	metaBucket       = []byte("meta")
	formatKey        = []byte("format")
	snapshotIndexKey = []byte("snapshot_index")
	snapshotBucket   = []byte("snapshot")
	blobKey          = []byte("blob")
	checksumKey      = []byte("sha256")
	logBucket        = []byte("log")

	legacyBucket      = []byte("control-plane-state")
	legacySnapshotKey = []byte("snapshot")

	logLayout = []string{string(logBucket), string(metaBucket), string(snapshotBucket)}
)

const format = 2

// checksumSize is the size of the CRC that ends a log entry.
const checksumSize = 4

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// errApplyInterrupted is the failure of a store whose apply of a committed log
// entry did not return.
var errApplyInterrupted = fmt.Errorf("%w: durable apply of a committed log entry did not finish", storage.ErrInternal)

func encodeUint64(n uint64) []byte { return binary.BigEndian.AppendUint64(make([]byte, 0, 8), n) }

func decodeUint64(b []byte) (uint64, bool) {
	if len(b) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(b), true
}

// entryChecksum covers the index as well as the command, so that an entry stored
// under another index fails it.
func entryChecksum(index uint64, command []byte) uint32 {
	return crc32.Update(crc32.Checksum(encodeUint64(index), castagnoli), castagnoli, command)
}

func encodeEntry(index uint64, command []byte) []byte {
	entry := make([]byte, len(command), len(command)+checksumSize)
	copy(entry, command)
	return binary.BigEndian.AppendUint32(entry, entryChecksum(index, command))
}

// decodeEntry returns the command of the log entry at index, after checking its CRC.
func decodeEntry(index uint64, entry []byte) ([]byte, error) {
	if len(entry) < checksumSize {
		return nil, fmt.Errorf("log entry %d is shorter than its checksum", index)
	}
	command := entry[:len(entry)-checksumSize]
	if binary.BigEndian.Uint32(entry[len(command):]) != entryChecksum(index, command) {
		return nil, fmt.Errorf("log entry %d: checksum mismatch", index)
	}
	return command, nil
}

// applyCommand applies the command of log entry index to dst: the decode and the
// ApplyOps the Raft state machine runs on a committed entry. A replay applies each
// entry with it; a commit runs the same two steps around the read lock.
func applyCommand(ctx context.Context, dst storage.Storage, index uint64, command []byte) error {
	cmd, err := overlay.DecodeCommand(command)
	if err != nil {
		return fmt.Errorf("log entry %d: %w", index, err)
	}
	if err := overlay.ApplyOps(ctx, dst, cmd.GetOps()); err != nil {
		return fmt.Errorf("log entry %d: %w", index, err)
	}
	return nil
}

func putSnapshot(bucket *bolt.Bucket, blob []byte) error {
	if err := bucket.Put(blobKey, blob); err != nil {
		return err
	}
	sum := sha256.Sum256(blob)
	return bucket.Put(checksumKey, sum[:])
}

// restore brings the database file to the log layout and loads the state it holds.
// A file bbolt initialized and never committed to, which a crash during its
// creation leaves, is initialized; a single-blob file is migrated. Either takes one
// bbolt transaction, which leaves the file as it was unless it commits. restore
// reports whether it initialized the file.
func (s *Store) restore() (bool, error) {
	var (
		buckets []string
		txID    int
	)
	err := s.db.View(func(tx *bolt.Tx) error {
		txID = tx.ID()
		return tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			buckets = append(buckets, string(name))
			return nil
		})
	})
	if err != nil {
		return false, err
	}
	created := false
	switch {
	case len(buckets) == 0 && txID <= 1:
		// bbolt writes transaction 1 when it creates a file, before any commit.
		created = true
		err = s.db.Update(initialize)
	case len(buckets) == 0:
		err = fmt.Errorf("database has no bucket after %d transactions", txID)
	case slices.Equal(buckets, []string{string(legacyBucket)}):
		err = s.db.Update(s.migrate)
	case !slices.Equal(buckets, logLayout):
		err = fmt.Errorf("unknown database layout with buckets %q", buckets)
	}
	if err != nil {
		return false, err
	}
	return created, s.db.View(s.load)
}

// initialize writes the log layout into a file without buckets: the snapshot of
// the empty state at index 0 and an empty log.
func initialize(tx *bolt.Tx) error {
	blob, err := snapshot.Marshal(inmemory.SnapshotContent{})
	if err != nil {
		return err
	}
	return createLayout(tx, blob)
}

// migrate turns a single-blob file into the log layout: its snapshot, checked and
// encoded at the current version, becomes the snapshot at index 0, with an empty
// log, and the single-blob bucket goes.
func (s *Store) migrate(tx *bolt.Tx) error {
	legacy := tx.Bucket(legacyBucket)
	data := legacy.Get(legacySnapshotKey)
	sum := sha256.Sum256(data)
	if !bytes.Equal(legacy.Get(checksumKey), sum[:]) {
		return errors.New("single-blob snapshot checksum mismatch")
	}
	content, err := snapshot.Unmarshal(data)
	if err != nil {
		return err
	}
	blob, err := snapshot.Marshal(content)
	if err != nil {
		return err
	}
	if err := createLayout(tx, blob); err != nil {
		return err
	}
	if h := s.hooks.migrate; h != nil {
		h()
	}
	return tx.DeleteBucket(legacyBucket)
}

// createLayout creates the buckets of the log layout, with blob as the snapshot at
// index 0 and no log entry.
func createLayout(tx *bolt.Tx, blob []byte) error {
	meta, err := tx.CreateBucket(metaBucket)
	if err != nil {
		return err
	}
	if err := meta.Put(formatKey, encodeUint64(format)); err != nil {
		return err
	}
	if err := meta.Put(snapshotIndexKey, encodeUint64(0)); err != nil {
		return err
	}
	bucket, err := tx.CreateBucket(snapshotBucket)
	if err != nil {
		return err
	}
	if err := putSnapshot(bucket, blob); err != nil {
		return err
	}
	_, err = tx.CreateBucket(logBucket)
	return err
}

// load publishes the state of a file in the log layout: the snapshot, checked
// against its sha256, with the log entries past it applied in order.
func (s *Store) load(tx *bolt.Tx) error {
	meta := tx.Bucket(metaBucket)
	if version, ok := decodeUint64(meta.Get(formatKey)); !ok || version != format {
		return fmt.Errorf("unsupported database format %x (want %d)", meta.Get(formatKey), format)
	}
	index, ok := decodeUint64(meta.Get(snapshotIndexKey))
	if !ok {
		return fmt.Errorf("invalid snapshot index %x", meta.Get(snapshotIndexKey))
	}
	bucket := tx.Bucket(snapshotBucket)
	blob := bucket.Get(blobKey)
	sum := sha256.Sum256(blob)
	if !bytes.Equal(bucket.Get(checksumKey), sum[:]) {
		return errors.New("snapshot checksum mismatch")
	}
	content, err := snapshot.Unmarshal(blob)
	if err != nil {
		return err
	}
	inner := inmemory.New()
	inner.RestoreFromSnapshot(content)
	last, size, err := replay(tx.Bucket(logBucket), index, inner)
	if err != nil {
		return err
	}
	s.inner, s.snapshotIndex, s.last, s.logBytes = inner, index, last, size
	return nil
}

// replay applies the log entries past index to dst in order and returns the last
// one and the bytes the entries take. An entry that is missing, at or below index,
// fails its CRC or fails to apply stops it: none is skipped.
func replay(log *bolt.Bucket, index uint64, dst storage.Storage) (last uint64, size int, err error) {
	last = index
	c := log.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		i, ok := decodeUint64(k)
		switch {
		case !ok:
			return 0, 0, fmt.Errorf("log key %x is not an index", k)
		case i <= index:
			return 0, 0, fmt.Errorf("log entry %d is at or below the snapshot index %d", i, index)
		case i != last+1:
			return 0, 0, fmt.Errorf("log entry %d is missing: the log goes on at %d", last+1, i)
		}
		command, err := decodeEntry(i, v)
		if err != nil {
			return 0, 0, err
		}
		if err := applyCommand(context.Background(), dst, i, command); err != nil {
			return 0, 0, err
		}
		last, size = i, size+len(v)
	}
	return last, size, nil
}

// commit appends ops to the log as the next entry and applies them to the published
// state. Nothing is published unless the entry committed. A failed commit or a
// failed apply of a committed entry fails the store. The caller holds writer.
func (s *Store) commit(ctx context.Context, ops []*raftv1.Op) error {
	command, err := overlay.EncodeCommand(ops)
	if err != nil {
		return fmt.Errorf("%w: durable command: %w", storage.ErrInternal, err)
	}
	index := s.last + 1
	entry := encodeEntry(index, command)
	if err := s.appendEntry(ctx, index, entry); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		// A failed sync can leave the outcome of the commit uncertain. Fail closed
		// until the next Open reads what the disk holds.
		return s.fail(fmt.Errorf("%w: durable commit of log entry %d: %w", storage.ErrInternal, index, err))
	}
	// The disk holds the entry from here on, so a failure leaves the published
	// state behind it. The operations come from the committed bytes, as a replay
	// reads them.
	cmd, err := overlay.DecodeCommand(command)
	if err != nil {
		return s.fail(fmt.Errorf("%w: durable apply of committed log entry %d: %v", storage.ErrInternal, index, err))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The entry is durable whatever ctx says now, and readers see none of its
	// operations until all of them are applied. The store counts as failed until
	// the apply returns, so an apply that panics part way publishes nothing.
	s.failure = errApplyInterrupted
	if h := s.hooks.committed; h != nil {
		h()
	}
	if err := overlay.ApplyOps(context.WithoutCancel(ctx), s.inner, cmd.GetOps()); err != nil {
		s.failure = fmt.Errorf("%w: durable apply of committed log entry %d: %v", storage.ErrInternal, index, err)
		return s.failure
	}
	s.failure = nil
	s.last, s.logBytes = index, s.logBytes+len(entry)
	return nil
}

// appendEntry stores entry under index, past the last entry of the log, in one bbolt
// transaction, which bbolt syncs before it returns. A context error before the
// commit rolls the transaction back.
func (s *Store) appendEntry(ctx context.Context, index uint64, entry []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		log := tx.Bucket(logBucket)
		if log == nil {
			return errors.New("missing log bucket")
		}
		key := encodeUint64(index)
		if last, _ := log.Cursor().Last(); last != nil && bytes.Compare(last, key) >= 0 {
			return fmt.Errorf("log entry %d does not follow the last entry %x", index, last)
		}
		// Keys only grow, so full pages split off and are never written again.
		log.FillPercent = 1
		if err := log.Put(key, entry); err != nil {
			return err
		}
		if h := s.hooks.commit; h != nil {
			h()
		}
		return ctx.Err()
	})
}
