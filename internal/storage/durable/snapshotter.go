package durable

import (
	"bytes"
	"context"
	"fmt"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
	bolt "go.etcd.io/bbolt"
)

// maxLogBytes is the size of the log entries past the snapshot that starts the
// next one, whatever their number.
const maxLogBytes = 16 << 20

// snapshotPoint is a point of a snapshot that the snapshot hook runs at.
type snapshotPoint int

const (
	// snapshotWritten: the snapshot and its index are written, the log is whole.
	snapshotWritten snapshotPoint = iota
	// logTruncated: the entries up to the snapshot are deleted, bbolt has not
	// committed.
	logTruncated
	// snapshotCommitted: bbolt committed the snapshot, the store has not counted it.
	snapshotCommitted
)

// snapshotter writes the snapshots of a store in a goroutine of its own, so no
// transaction runs one. A commit wakes it when the log crosses a trigger; it then
// waits for the writer like a transaction does.
type snapshotter struct {
	wake chan struct{}
	stop context.CancelFunc
	done chan struct{}
}

func (s *Store) startSnapshotter() {
	ctx, stop := context.WithCancel(context.Background())
	s.snapshotter = snapshotter{wake: make(chan struct{}, 1), stop: stop, done: make(chan struct{})}
	go func() {
		defer close(s.snapshotter.done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.snapshotter.wake:
				// A failed snapshot fails the store, whose next call reports it.
				_ = s.snapshotIfDue(ctx)
			}
		}
	}()
}

// stopSnapshotter stops the snapshotter and waits for a snapshot it is writing.
func (s *Store) stopSnapshotter() {
	s.snapshotter.stop()
	<-s.snapshotter.done
}

// due reports whether the log past the snapshot holds the threshold of entries or
// maxLogBytes. The caller holds writer.
func (s *Store) due() bool {
	return s.last-s.snapshotIndex >= uint64(s.threshold) || s.logBytes >= maxLogBytes
}

// wakeSnapshotter starts a snapshot in the background when one is due. The caller
// holds writer.
func (s *Store) wakeSnapshotter() {
	if !s.due() {
		return
	}
	select {
	case s.snapshotter.wake <- struct{}{}:
	default:
	}
}

func (s *Store) snapshotIfDue(ctx context.Context) error {
	if err := s.writer.Acquire(ctx, 1); err != nil {
		return err
	}
	defer s.writer.Release(1)
	if !s.due() {
		return nil
	}
	return s.snapshotLocked()
}

// snapshotLocked writes the published state as the snapshot at the last log entry,
// and deletes the entries up to it, in one bbolt transaction: a crash leaves the
// old snapshot and the whole log, or the new snapshot and the rest of the log. The
// caller holds writer, so the state and the log hold still. A failure fails the
// store, since the outcome of a failed commit is uncertain.
func (s *Store) snapshotLocked() error {
	if err := s.serving(); err != nil {
		return err
	}
	index := s.last
	if index == s.snapshotIndex {
		return nil
	}
	blob, err := snapshot.Marshal(s.inner.SnapshotForRaft())
	if err != nil {
		return s.fail(fmt.Errorf("%w: durable snapshot at log entry %d: %w", storage.ErrInternal, index, err))
	}
	var size int
	err = s.db.Update(func(tx *bolt.Tx) error {
		key := encodeUint64(index)
		log := tx.Bucket(logBucket)
		if last, _ := log.Cursor().Last(); !bytes.Equal(last, key) {
			return fmt.Errorf("the log ends at %x, not at log entry %d", last, index)
		}
		if err := putSnapshot(tx.Bucket(snapshotBucket), blob); err != nil {
			return err
		}
		if err := tx.Bucket(metaBucket).Put(snapshotIndexKey, key); err != nil {
			return err
		}
		s.hook(snapshotWritten)
		var err error
		if size, err = truncate(log, key); err != nil {
			return err
		}
		s.hook(logTruncated)
		return nil
	})
	if err != nil {
		return s.fail(fmt.Errorf("%w: durable snapshot at log entry %d: %w", storage.ErrInternal, index, err))
	}
	s.hook(snapshotCommitted)
	s.snapshotIndex, s.logBytes = index, s.logBytes-size
	return nil
}

func (s *Store) hook(p snapshotPoint) {
	if h := s.hooks.snapshot; h != nil {
		h(p)
	}
}

// truncate deletes the log entries up to the key last and returns the bytes they
// took.
func truncate(log *bolt.Bucket, last []byte) (int, error) {
	var (
		keys [][]byte
		size int
	)
	c := log.Cursor()
	for k, v := c.First(); k != nil && bytes.Compare(k, last) <= 0; k, v = c.Next() {
		keys = append(keys, bytes.Clone(k))
		size += len(v)
	}
	for _, k := range keys {
		if err := log.Delete(k); err != nil {
			return 0, err
		}
	}
	return size, nil
}
