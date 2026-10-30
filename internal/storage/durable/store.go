// Package durable provides single-process disk-backed standalone storage.
// A transaction's writes are committed to a log in bbolt, synced, before they are
// applied to the state readers see, and a snapshot of that state replaces the
// log from time to time. bbolt's file lock excludes a second Podium process using
// the same directory.
package durable

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sync/semaphore"
)

const FileName = "podium.db"

// DefaultSnapshotThreshold is the number of log entries past the snapshot that
// starts the next one, unless WithSnapshotThreshold sets another.
const DefaultSnapshotThreshold = 4096

var _ storage.Storage = (*Store)(nil)

// Store logs the operations of each transaction and applies them to the published
// state once the log commit is synced. Writers are serialized: a transaction holds
// writer across its callback, its commit and its apply, so the log order is the
// apply order and each transaction reads the latest published state. Readers hold
// mu shared, which a writer takes exclusively only to apply a committed command,
// so a read never waits for a sync.
type Store struct {
	// writer admits one transaction or snapshot at a time. Waiting for it honours
	// the caller's context.
	writer *semaphore.Weighted

	// mu guards the published state and whether the store serves.
	mu sync.RWMutex
	// inner is the published state. It changes only under mu and writer.
	inner *inmemory.Store
	// filters is an immutable empty factory; builders use criteria, never state.
	filters *inmemory.Store
	db      *bolt.DB
	closed  bool
	// failure is why the store stopped serving: a commit, an apply or a snapshot
	// that failed, after which memory and disk may disagree until Open. reported
	// records that fail passed it to onFailure.
	failure  error
	reported bool
	// onFailure is the failure handler of WithFailureHandler, or nil.
	onFailure func(error)

	// The log, guarded by writer: the last committed entry, the entry the
	// snapshot holds the state at, and the bytes of the entries past it.
	last, snapshotIndex uint64
	logBytes            int
	threshold           int

	snapshotter snapshotter
	hooks       hooks
}

// Option configures Open.
type Option func(*options)

type options struct {
	snapshotThreshold int
	onFailure         func(error)
	hooks             hooks
}

// WithSnapshotThreshold sets the number of log entries past the snapshot that
// starts the next one. Zero keeps DefaultSnapshotThreshold; a negative number
// fails Open.
func WithSnapshotThreshold(n int) Option {
	return func(o *options) { o.snapshotThreshold = n }
}

// WithFailureHandler sets a function that hears once, as it happens, why the
// store stopped serving: a failed commit, a committed entry whose apply failed or
// panicked, or a failed snapshot. It runs on the goroutine that failed, after the
// store's lock is released, while that transaction or snapshot still holds the
// writer. So it may call the read methods, which return the failure, and must not
// call WithTx, a write method or Close, which would wait for that writer.
func WithFailureHandler(fn func(error)) Option {
	return func(o *options) { o.onFailure = fn }
}

// hooks are the seams tests stall the store or end the process at. Production
// leaves them nil.
type hooks struct {
	// commit runs in the bbolt transaction that appends a command, before bbolt
	// commits it.
	commit func()
	// committed runs once a command is committed, before the store applies it,
	// with mu held exclusively: it must not call the store.
	committed func()
	// snapshot runs at each point of a snapshot.
	snapshot func(snapshotPoint)
	// migrate runs in the migration transaction once the log layout is written,
	// before the single-blob bucket is deleted.
	migrate func()
}

// Open restores standalone state or creates a private database in dataDir.
// Corrupt, incompatible or insecure existing files fail startup without reseeding.
// The directory must be private to this single standalone deployment.
func Open(dataDir string, opts ...Option) (*Store, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	switch {
	case o.snapshotThreshold == 0:
		o.snapshotThreshold = DefaultSnapshotThreshold
	case o.snapshotThreshold < 0:
		return nil, fmt.Errorf("%w: durable: snapshot threshold must not be negative", storage.ErrInvalidArgument)
	}
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
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return nil, fmt.Errorf("durable: stat: %w", err)
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("durable: existing database is not a regular file")
	case info.Size() == 0:
		return nil, fmt.Errorf("durable: %s is empty and holds no state (a crash while the first start creates it can leave it so): if this deployment never started, move the file aside and start again; otherwise restore a backup", path)
	case info.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("durable: existing database must have owner-only permissions")
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 250 * time.Millisecond})
	if err != nil {
		return nil, fmt.Errorf("durable: open database: %w", err)
	}
	s := &Store{
		writer:    semaphore.NewWeighted(1),
		filters:   inmemory.New(),
		db:        db,
		threshold: o.snapshotThreshold,
		onFailure: o.onFailure,
		hooks:     o.hooks,
	}
	created, err := s.restore()
	if err == nil && created {
		// The first file creation also needs the containing directory entry synced.
		var directory *os.File
		if directory, err = os.Open(dataDir); err == nil {
			err = directory.Sync()
			_ = directory.Close()
		}
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("durable: restore: %w", err)
	}
	s.startSnapshotter()
	return s, nil
}

// WithTx runs fn on a transaction over the published state, then appends the
// operations it recorded to the log as one entry, which bbolt syncs, and applies
// them to the published state. A transaction that recorded nothing touches no
// disk. Waiting for the writer respects ctx. Callback errors and cancellation
// observed before the commit change nothing. Once bbolt starts the commit, a
// successful commit wins cancellation and the operations are applied. A commit
// error, or an apply error after a commit, publishes nothing and makes the store
// unavailable until restart: memory and disk may then disagree.
func (s *Store) WithTx(ctx context.Context, fn func(tx storage.Storage) error) error {
	if fn == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.writer.Acquire(ctx, 1); err != nil {
		return err
	}
	defer s.writer.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.serving(); err != nil {
		return err
	}
	// writer keeps every other writer out, so the base holds still while fn runs.
	tx := overlay.New(s.inner)
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tx.Empty() {
		return nil
	}
	if err := s.commit(ctx, tx.Ops()); err != nil {
		return err
	}
	s.wakeSnapshotter()
	return nil
}

// Close stops the snapshotter, waits for an in-flight transaction or snapshot and
// releases the database lock. Acknowledged writes are already on disk. It is
// idempotent.
func (s *Store) Close() error {
	s.stopSnapshotter()
	if err := s.writer.Acquire(context.Background(), 1); err != nil {
		return err
	}
	defer s.writer.Release(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// serving returns the error of a call to a closed or failed store, and nil for a
// store that serves.
func (s *Store) serving() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.servingLocked()
}

// servingLocked is serving for a caller that holds mu.
func (s *Store) servingLocked() error {
	switch {
	case s.closed:
		return storage.ErrUnavailable
	case s.failure != nil:
		return fmt.Errorf("%w: %v", storage.ErrUnavailable, s.failure)
	}
	return nil
}

// fail stops the store from serving with err as the reason, unless a failure is
// recorded already, and returns err. The first failure is the one kept, and fail
// passes it to the failure handler once, after it releases mu: a caller must not
// hold mu, and the handler may read the store.
func (s *Store) fail(err error) error {
	s.mu.Lock()
	if s.failure == nil {
		s.failure = err
	}
	failure, report := s.failure, !s.reported
	s.reported = true
	s.mu.Unlock()
	if report && s.onFailure != nil {
		s.onFailure(failure)
	}
	return err
}

// readState reads the published state under the read lock. A writer takes the lock
// only to apply a committed command, so a read waits for neither a transaction's
// callback nor its sync.
func readState[T any](ctx context.Context, s *Store, read func(*inmemory.Store) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.servingLocked(); err != nil {
		return zero, err
	}
	value, err := read(s.inner)
	if canceled := ctx.Err(); canceled != nil {
		return zero, canceled
	}
	return value, err
}
