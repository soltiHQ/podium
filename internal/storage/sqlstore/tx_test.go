package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// errCallback is the error a test's callback fails with.
var errCallback = errors.New("sqlstore test: the callback fails")

// TestTxRollbackOnError checks that a callback error rolls the transaction
// back, after the callback, which ran once, read its own write.
func TestTxRollbackOnError(t *testing.T) {
	db := migrated(t)
	createNotes(t, db, "kept")
	s, ctx := openStore(t, Options{Primary: db}), t.Context()

	calls := 0
	err := s.transact(ctx, func(h *handle) error {
		calls++
		if err := addNote(ctx, h.tx, "rolled-back"); err != nil {
			return err
		}
		wantNotes(t, "inside the transaction", h.tx, "kept", "rolled-back")
		return errCallback
	})
	if !errors.Is(err, errCallback) || calls != 1 {
		t.Errorf("transact: %v after %d calls; want the callback's error after one", err, calls)
	}
	wantNotes(t, "after the rollback", db, "kept")
}

// TestTxRollbackOnCancel checks that a context done before the commit rolls
// back, that one done before the transaction runs no callback, and that the
// store goes on after both.
func TestTxRollbackOnCancel(t *testing.T) {
	db := migrated(t)
	createNotes(t, db)
	s := openStore(t, Options{Primary: db})

	t.Run("a context done before the commit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		err := s.transact(ctx, func(h *handle) error {
			if err := addNote(ctx, h.tx, "cancelled"); err != nil {
				return err
			}
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("transact: %v, want context.Canceled", err)
		}
		wantNotes(t, "after the cancelled commit", db)
	})

	t.Run("a context done before the transaction", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		calls := 0
		err := s.transact(ctx, func(*handle) error { calls++; return nil })
		if !errors.Is(err, context.Canceled) || calls != 0 {
			t.Errorf("transact: %v after %d calls; want context.Canceled and no call", err, calls)
		}
	})

	t.Run("the store goes on", func(t *testing.T) {
		ctx := t.Context()
		if err := s.transact(ctx, func(h *handle) error { return addNote(ctx, h.tx, "after") }); err != nil {
			t.Fatalf("transact: %v", err)
		}
		wantNotes(t, "after the commit", db, "after")
	})
}

// TestTxPanic checks that a panic in the callback rolls the transaction back,
// releases the gate and the connection, and is raised again.
func TestTxPanic(t *testing.T) {
	db := migrated(t)
	createNotes(t, db, "kept")
	s, ctx := openStore(t, Options{Primary: db}), t.Context()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = s.transact(ctx, func(h *handle) error {
			if err := addNote(ctx, h.tx, "lost"); err != nil {
				return err
			}
			panic("sqlstore test: the callback panics")
		})
		t.Error("transact returned after the callback's panic")
	}()
	if recovered != "sqlstore test: the callback panics" {
		t.Errorf("recovered %v, want the callback's panic raised again", recovered)
	}
	wantNotes(t, "after the panic", db, "kept")
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Errorf("%d connections in use after the panic, want none", inUse)
	}

	// The gate is free: a leaked one would hold this transaction until its
	// context ends.
	next, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.transact(next, func(h *handle) error { return addNote(next, h.tx, "after") }); err != nil {
		t.Fatalf("transact after the panic: %v", err)
	}
	wantNotes(t, "after the next transaction", db, "after", "kept")
}

// TestTxNested checks that a transaction inside a callback is the outer one:
// each sees the other's writes, the nested callback's error rolls nothing back
// by itself, the outer error rolls back the nested writes, and a nil callback is
// refused.
func TestTxNested(t *testing.T) {
	db := migrated(t)
	createNotes(t, db)
	s, ctx := openStore(t, Options{Primary: db}), t.Context()

	err := s.transact(ctx, func(h *handle) error {
		if err := addNote(ctx, h.tx, "outer"); err != nil {
			return err
		}
		if err := h.transact(ctx, func(nested *handle) error {
			if nested != h {
				return errors.New("the nested transaction is not the outer one")
			}
			wantNotes(t, "inside the nested transaction", nested.tx, "outer")
			return addNote(ctx, nested.tx, "inner")
		}); err != nil {
			return err
		}
		wantNotes(t, "after the nested transaction", h.tx, "inner", "outer")
		// There is no savepoint: the nested callback's error rolls nothing back.
		if err := h.transact(ctx, func(nested *handle) error {
			if err := addNote(ctx, nested.tx, "failed"); err != nil {
				return err
			}
			return errCallback
		}); !errors.Is(err, errCallback) {
			return fmt.Errorf("the nested transaction returned %v, want its callback's error", err)
		}
		if err := h.transact(ctx, nil); !errors.Is(err, storage.ErrInvalidArgument) {
			return fmt.Errorf("a nested transaction of a nil callback returned %v, want storage.ErrInvalidArgument", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transact: %v", err)
	}
	wantNotes(t, "after the commit", db, "failed", "inner", "outer")

	err = s.transact(ctx, func(h *handle) error {
		if err := h.transact(ctx, func(nested *handle) error { return addNote(ctx, nested.tx, "late") }); err != nil {
			return err
		}
		return errCallback
	})
	if !errors.Is(err, errCallback) {
		t.Errorf("transact: %v, want the callback's error", err)
	}
	wantNotes(t, "after the outer rollback", db, "failed", "inner", "outer")
	if err := s.transact(ctx, nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("transact of a nil callback: %v, want storage.ErrInvalidArgument", err)
	}
}

// stubFence records its checks; it refuses with err, or, with none, marks the
// transaction it checks, so that the callback can tell the check ran in its
// transaction before it.
type stubFence struct {
	err    error
	checks atomic.Int32
}

func (f *stubFence) Check(ctx context.Context, tx *sql.Tx) error {
	f.checks.Add(1)
	if f.err != nil {
		return f.err
	}
	_, err := tx.ExecContext(ctx, `SELECT set_config('podium.fenced', 'yes', true)`)
	return err
}

// writingFence writes a note before it refuses, as a fence that took a lock or
// counted a refusal would.
type writingFence struct{ err error }

func (f writingFence) Check(ctx context.Context, tx *sql.Tx) error {
	if err := addNote(ctx, tx, "written by the fence"); err != nil {
		return err
	}
	return f.err
}

// fenced reports whether the stub fence marked the transaction q runs in.
func fenced(ctx context.Context, q querier) (bool, error) {
	var mark string
	err := q.QueryRowContext(ctx, `SELECT coalesce(current_setting('podium.fenced', true), '')`).Scan(&mark)
	return mark == "yes", err
}

// TestFence checks that the fence checks each write transaction once, inside
// it and before its callback, and that a refusing fence's error is returned
// unchanged, with no callback run and nothing written, what the fence itself
// wrote included.
func TestFence(t *testing.T) {
	db := migrated(t)
	createNotes(t, db)
	ctx := t.Context()

	t.Run("checks every write transaction before its callback", func(t *testing.T) {
		fence := &stubFence{}
		s := openStore(t, Options{Primary: db, Fence: fence})
		err := s.transact(ctx, func(h *handle) error {
			if ok, err := fenced(ctx, h.tx); err != nil || !ok {
				return fmt.Errorf("the callback runs in a transaction the fence did not check first: %v", err)
			}
			// A nested transaction is the outer one, checked once.
			return h.transact(ctx, func(nested *handle) error { return addNote(ctx, nested.tx, "a") })
		})
		if err != nil || fence.checks.Load() != 1 {
			t.Fatalf("transact: %v after %d checks; want one check", err, fence.checks.Load())
		}
		if err := s.transact(ctx, func(h *handle) error { return addNote(ctx, h.tx, "b") }); err != nil || fence.checks.Load() != 2 {
			t.Fatalf("a second transaction: %v after %d checks; want two", err, fence.checks.Load())
		}
		wantNotes(t, "after the commits", db, "a", "b")
	})

	t.Run("a refusal is returned unchanged", func(t *testing.T) {
		errRefused := errors.New("sqlstore test: this replica does not lead")
		fence := &stubFence{err: errRefused}
		s := openStore(t, Options{Primary: db, Fence: fence})
		calls := 0
		err := s.transact(ctx, func(h *handle) error { calls++; return addNote(ctx, h.tx, "refused") })
		// The fence's own error, not one that wraps it.
		if err != errRefused || calls != 0 || fence.checks.Load() != 1 {
			t.Errorf("transact: %v after %d calls and %d checks; want the fence's error unchanged, no call, one check", err, calls, fence.checks.Load())
		}
		wantNotes(t, "after the refusal", db, "a", "b")
	})

	t.Run("a refusal rolls back what the fence wrote", func(t *testing.T) {
		errRefused := errors.New("sqlstore test: this replica lost the lease")
		s := openStore(t, Options{Primary: db, Fence: writingFence{err: errRefused}})
		calls := 0
		err := s.transact(ctx, func(*handle) error { calls++; return nil })
		if err != errRefused || calls != 0 {
			t.Errorf("transact: %v after %d calls; want the fence's error unchanged and no call", err, calls)
		}
		wantNotes(t, "after the refusal", db, "a", "b")
	})

	t.Run("no fence allows every write", func(t *testing.T) {
		s := openStore(t, Options{Primary: db})
		if _, ok := s.fence.(AllowAll); !ok {
			t.Errorf("the fence of a store opened without one is %T, want AllowAll", s.fence)
		}
		if err := s.transact(ctx, func(h *handle) error { return addNote(ctx, h.tx, "c") }); err != nil {
			t.Errorf("transact: %v", err)
		}
	})
}

// TestWriteGate checks that one write transaction runs at a time in a store,
// and that waiting for the gate honours the context.
func TestWriteGate(t *testing.T) {
	db := migrated(t)
	createNotes(t, db)
	s, ctx := openStore(t, Options{Primary: db}), t.Context()

	t.Run("one write transaction at a time", func(t *testing.T) {
		var active, most atomic.Int32
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				err := s.transact(ctx, func(h *handle) error {
					raise(&most, active.Add(1))
					defer active.Add(-1)
					return addNote(ctx, h.tx, fmt.Sprintf("note-%d", i))
				})
				if err != nil {
					t.Errorf("transact %d: %v", i, err)
				}
			})
		}
		wg.Wait()
		if most.Load() != 1 {
			t.Errorf("%d write transactions ran at once, want one", most.Load())
		}
		wantNotes(t, "after the transactions", db,
			"note-0", "note-1", "note-2", "note-3", "note-4", "note-5", "note-6", "note-7")
	})

	t.Run("waiting for the gate honours the context", func(t *testing.T) {
		held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			done <- s.transact(ctx, func(*handle) error { close(held); <-release; return nil })
		}()
		select {
		case <-held:
		case err := <-done:
			t.Fatalf("the first transaction ended before it held the gate: %v", err)
		}

		waiting, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		calls := 0
		err := s.transact(waiting, func(*handle) error { calls++; return nil })
		if !errors.Is(err, context.DeadlineExceeded) || calls != 0 {
			t.Errorf("transact while the gate is held: %v after %d calls; want context.DeadlineExceeded and no call", err, calls)
		}

		close(release)
		if err := <-done; err != nil {
			t.Fatalf("the first transaction: %v", err)
		}
		if err := s.transact(ctx, func(*handle) error { return nil }); err != nil {
			t.Errorf("transact once the gate is free: %v", err)
		}
	})
}

// raise sets most to n when n is larger.
func raise(most *atomic.Int32, n int32) {
	for m := most.Load(); n > m; m = most.Load() {
		if most.CompareAndSwap(m, n) {
			return
		}
	}
}
