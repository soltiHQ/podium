// Package durable provides single-process disk-backed standalone storage.
// Writes commit a versioned state snapshot in bbolt before publishing memory.
// bbolt's file lock excludes a second Podium process using the same directory.
package durable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sync/semaphore"
)

const FileName = "podium.db"

// Readers share admission; a writer takes all weight. FIFO weighted acquisition
// prevents new readers from starving a queued transaction.
const admissionWeight int64 = 1 << 30

var (
	stateBucket = []byte("control-plane-state")
	snapshotKey = []byte("snapshot")
	checksumKey = []byte("sha256")
)

var _ storage.Storage = (*Store)(nil)

// Store serializes transactions and publishes state only after the durable commit.
// Each transaction rewrites the complete snapshot; cost is proportional to state
// size. Large or high-write deployments should use the existing Raft backend.
type Store struct {
	mu        sync.RWMutex
	admission *semaphore.Weighted
	// filters is an immutable empty factory; builders use criteria, never state.
	filters *inmemory.Store
	inner   *inmemory.Store
	db      *bolt.DB
	closed  bool
	failed  bool
}

// Open restores standalone state or creates a private database in dataDir.
// Corrupt, incompatible or insecure existing files fail startup without reseeding.
// The directory must be private to this single standalone deployment.
func Open(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("%w: durable: data directory is required", storage.ErrInvalidArgument)
	}
	dataDir = filepath.Clean(dataDir)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("durable: mkdir: %w", err)
	}
	dirInfo, err := os.Lstat(dataDir)
	if err != nil {
		return nil, fmt.Errorf("durable: stat directory: %w", err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("durable: data directory must be a private regular directory, not a symlink")
	}
	path := filepath.Join(dataDir, FileName)
	newFile := false
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		newFile = true
	case err != nil:
		return nil, fmt.Errorf("durable: stat: %w", err)
	case !info.Mode().IsRegular() || info.Size() == 0:
		return nil, fmt.Errorf("durable: existing database is not a nonempty regular file")
	case info.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("durable: existing database must have owner-only permissions")
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 250 * time.Millisecond})
	if err != nil {
		return nil, fmt.Errorf("durable: open database: %w", err)
	}
	s := &Store{inner: inmemory.New(), filters: inmemory.New(), db: db, admission: semaphore.NewWeighted(admissionWeight)}
	var content inmemory.SnapshotContent
	err = db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(stateBucket)
		if bucket == nil {
			if !newFile {
				return fmt.Errorf("durable: missing state bucket")
			}
			var err error
			bucket, err = tx.CreateBucket(stateBucket)
			if err != nil {
				return err
			}
			data, err := snapshot.Marshal(content)
			if err != nil {
				return err
			}
			return putSnapshot(bucket, data)
		}
		data := bucket.Get(snapshotKey)
		sum := sha256.Sum256(data)
		if !bytes.Equal(bucket.Get(checksumKey), sum[:]) {
			return fmt.Errorf("durable: snapshot checksum mismatch")
		}
		var err error
		content, err = snapshot.Unmarshal(data)
		return err
	})
	if err == nil && newFile {
		// The first file creation also needs the containing directory entry synced.
		directory, openErr := os.Open(dataDir)
		if openErr != nil {
			err = openErr
		} else {
			err = directory.Sync()
			_ = directory.Close()
		}
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("durable: restore: %w", err)
	}
	s.inner.RestoreFromSnapshot(content)
	return s, nil
}

func putSnapshot(bucket *bolt.Bucket, data []byte) error {
	if err := bucket.Put(snapshotKey, data); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	return bucket.Put(checksumKey, sum[:])
}

// WithTx stages changes in a private store and fsyncs the database transaction
// before publishing them. Waiting admission respects ctx. Callback errors and
// cancellation observed before native commit leave memory unchanged. Once bbolt
// starts native commit, successful commit wins cancellation and publishes the full
// state. A disk commit error leaves memory unchanged and makes the store unavailable
// until restart because its disk outcome may be uncertain.
func (s *Store) WithTx(ctx context.Context, fn func(tx storage.Storage) error) error {
	if fn == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.admission.Acquire(ctx, admissionWeight); err != nil {
		return err
	}
	defer s.admission.Release(admissionWeight)
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed {
		return storage.ErrUnavailable
	}
	candidate := inmemory.New()
	candidate.RestoreFromSnapshot(s.inner.SnapshotForRaft())
	if err := candidate.WithTx(ctx, fn); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := snapshot.Marshal(candidate.SnapshotForRaft())
	if err != nil {
		return fmt.Errorf("%w: durable snapshot: %w", storage.ErrInternal, err)
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		bucket := tx.Bucket(stateBucket)
		if bucket == nil {
			return fmt.Errorf("missing state bucket")
		}
		if err := putSnapshot(bucket, data); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		// A failed fsync can leave the disk commit outcome uncertain. Fail closed
		// until the next Open validates and restores the durable state.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		s.failed = true
		return fmt.Errorf("%w: durable commit: %w", storage.ErrInternal, err)
	}
	s.inner = candidate
	return nil
}

// Close releases the database lock. Acknowledged writes are already on disk.
// It is idempotent and waits for an in-flight transaction to finish.
func (s *Store) Close() error {
	if err := s.admission.Acquire(context.Background(), admissionWeight); err != nil {
		return err
	}
	defer s.admission.Release(admissionWeight)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// readState admits concurrent reads without making a canceled caller wait for a
// disk commit. mu still protects the published fields and test snapshots.
func readState[T any](ctx context.Context, s *Store, read func(*inmemory.Store) (T, error)) (T, error) {
	var zero T
	if err := s.admission.Acquire(ctx, 1); err != nil {
		return zero, err
	}
	defer s.admission.Release(1)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if s.closed || s.failed {
		return zero, storage.ErrUnavailable
	}
	value, err := read(s.inner)
	if canceled := ctx.Err(); canceled != nil {
		return zero, canceled
	}
	return value, err
}
