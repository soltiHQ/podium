package inmemory

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestSessionClockedRotationInsideTransaction(t *testing.T) {
	ctx := context.Background()
	s := New()
	original := mkSession(t, "session-clock", "u", "c", enum.Password)
	original.SetUpdatedAt(fixedNow())
	requireNoErr(t, s.CreateSession(ctx, original))
	epoch := time.Unix(0, 0)
	requireNoErr(t, s.WithTx(ctx, func(tx storage.Storage) error {
		clocked, ok := tx.(storage.ClockedSessionRotation)
		if !ok {
			t.Fatal("transaction lost clock support")
		}
		if err := clocked.RotateRefreshAt(ctx, original.ID(), []byte("new"), epoch, epoch); err != nil {
			return err
		}
		got, err := tx.GetSession(ctx, original.ID())
		if err != nil {
			return err
		}
		if !got.UpdatedAt().Equal(epoch) || !got.ExpiresAt().Equal(epoch) || !bytes.Equal(got.RefreshHash(), []byte("new")) {
			t.Fatal("explicit clock state missing")
		}
		return nil
	}))
	got, err := s.GetSession(ctx, original.ID())
	requireNoErr(t, err)
	if !got.UpdatedAt().Equal(epoch) {
		t.Fatal("epoch clock not committed")
	}
	requireNoErr(t, s.RotateRefreshAt(ctx, original.ID(), []byte("legacy"), fixedNow(), time.Time{}))
	got, err = s.GetSession(ctx, original.ID())
	requireNoErr(t, err)
	if !got.UpdatedAt().Equal(epoch) {
		t.Fatal("legacy fallback invented clock")
	}
}

func TestSessionClockedRotationCancellationDoesNotMutate(t *testing.T) {
	ctx := context.Background()
	s := New()
	original := mkSession(t, "session-clock", "u", "c", enum.Password)
	requireNoErr(t, s.CreateSession(ctx, original))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.RotateRefreshAt(canceled, original.ID(), []byte("new"), fixedNow(), fixedNow()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled rotation: %v", err)
	}
	got, err := s.GetSession(ctx, original.ID())
	requireNoErr(t, err)
	if !bytes.Equal(got.RefreshHash(), original.RefreshHash()) || !got.UpdatedAt().Equal(original.UpdatedAt()) {
		t.Fatal("canceled rotation mutated state")
	}
}
