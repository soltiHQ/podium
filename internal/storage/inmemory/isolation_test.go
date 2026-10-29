package inmemory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestTransactionRollbackDoesNotLoseConcurrentWrite(t *testing.T) {
	s := New()
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	txDone := make(chan error, 1)
	go func() {
		txDone <- s.WithTx(ctx, func(tx storage.Storage) error {
			if err := tx.UpsertAgent(ctx, mkAgent(t, "transient")); err != nil {
				return err
			}
			close(entered)
			<-release
			return errors.New("abort")
		})
	}()
	<-entered
	writeDone := make(chan error, 1)
	go func() { writeDone <- s.UpsertAgent(ctx, mkAgent(t, "concurrent")) }()
	select {
	case <-writeDone:
		t.Fatal("direct write bypassed transaction gate")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-txDone
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAgent(ctx, "concurrent"); err != nil {
		t.Fatalf("concurrent committed write lost: %v", err)
	}
	if _, err := s.GetAgent(ctx, "transient"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("uncommitted agent leaked: %v", err)
	}
}

func TestTransactionReadsCannotObserveIntermediateWrites(t *testing.T) {
	s := New()
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithTx(ctx, func(tx storage.Storage) error {
			_ = tx.UpsertAgent(ctx, mkAgent(t, "uncommitted"))
			close(entered)
			<-release
			return errors.New("abort")
		})
	}()
	<-entered
	read := make(chan error, 1)
	go func() { _, err := s.GetAgent(ctx, "uncommitted"); read <- err }()
	select {
	case err := <-read:
		t.Fatalf("read observed transaction intermediate state: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-done
	if err := <-read; !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want absent after rollback: %v", err)
	}
}

func TestCancelledTransactionRollsBackNilCallback(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	err := s.WithTx(ctx, func(tx storage.Storage) error {
		if err := tx.UpsertAgent(ctx, mkAgent(t, "cancelled")); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	if _, err := s.GetAgent(context.Background(), "cancelled"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("cancelled write committed")
	}
}

func TestSnapshotIsConsistentAndDetached(t *testing.T) {
	s := New()
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, mkAgent(t, "a"))
	snap := s.SnapshotForRaft()
	snap.Agents[0].LabelAdd("leak", "bad")
	a, _ := s.GetAgent(ctx, "a")
	if _, ok := a.Label("leak"); ok {
		t.Fatal("snapshot mutates live state")
	}
	s.RestoreFromSnapshot(snap)
	snap.Agents[0].LabelAdd("later", "bad")
	a, _ = s.GetAgent(ctx, "a")
	if _, ok := a.Label("later"); ok {
		t.Fatal("restore retains caller aliases")
	}
}
