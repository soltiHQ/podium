package raft

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"google.golang.org/protobuf/proto"
)

func clockSession(t *testing.T) *model.Session {
	t.Helper()
	expires := time.Date(2030, 1, 1, 0, 0, 0, 91, time.UTC)
	s, err := model.NewSession("session-clock", "user-clock", "credential-clock", enum.Password, []byte("original-hash"), expires)
	if err != nil {
		t.Fatal(err)
	}
	s.SetCreatedAt(time.Date(2025, 1, 2, 3, 4, 5, 97, time.UTC))
	s.SetUpdatedAt(s.CreatedAt())
	return s
}

func clockCommand(t *testing.T, op *raftv1.Op) []byte {
	t.Helper()
	data, err := encodeCommand([]*raftv1.Op{op})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func applyClockCommand(t *testing.T, f *FSM, data []byte, index uint64) {
	t.Helper()
	if result := f.Apply(&hraft.Log{Data: append([]byte(nil), data...), Index: index}); result != nil {
		t.Fatalf("apply[%d]: %v", index, result)
	}
}

func TestFSMSessionClockRevokeReplication(t *testing.T) {
	original := clockSession(t)
	revokedAt := original.CreatedAt().Add(time.Hour)
	assertSessionClockReplicas(t, original, &raftv1.Op{Op: &raftv1.Op_SessionRevoke{SessionRevoke: &raftv1.SessionRevokeMsg{Id: original.ID(), RevokedAtNs: revokedAt.UnixNano()}}}, revokedAt)
}

func TestFSMSessionClockLegacyRotateReplication(t *testing.T) {
	original := clockSession(t)
	assertSessionClockReplicas(t, original, &raftv1.Op{Op: &raftv1.Op_SessionRotateRefresh{SessionRotateRefresh: &raftv1.SessionRotateRefreshMsg{Id: original.ID(), RefreshHash: []byte("rotated-hash"), ExpiresAtNs: original.ExpiresAt().Add(time.Hour).UnixNano()}}}, original.UpdatedAt())
}

func assertSessionClockReplicas(t *testing.T, original *model.Session, mutation *raftv1.Op, want time.Time) {
	t.Helper()
	create := clockCommand(t, &raftv1.Op{Op: &raftv1.Op_SessionCreate{SessionCreate: wire.SessionToProto(original)}})
	data := clockCommand(t, mutation)
	var replicas []*model.Session
	for i := 0; i < 2; i++ {
		store := inmemory.New()
		f := NewFSM(store, nil)
		applyClockCommand(t, f, create, 1)
		applyClockCommand(t, f, data, 2)
		got, err := store.GetSession(context.Background(), original.ID())
		if err != nil {
			t.Fatal(err)
		}
		replicas = append(replicas, got)
		if !got.UpdatedAt().Equal(want) {
			t.Errorf("replica[%d] UpdatedAt=%s; want authoritative %s", i, got.UpdatedAt().Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !proto.Equal(wire.SessionToProto(replicas[0]), wire.SessionToProto(replicas[1])) {
		t.Errorf("same committed bytes diverged across delayed replicas: UpdatedAtNs=%d/%d", replicas[0].UpdatedAt().UnixNano(), replicas[1].UpdatedAt().UnixNano())
	}
}

type sessionClockSink struct{ bytes.Buffer }

func (s *sessionClockSink) ID() string    { return "session-clock" }
func (s *sessionClockSink) Close() error  { return nil }
func (s *sessionClockSink) Cancel() error { return nil }

func restoreSessionClock(t *testing.T, f *FSM, id string) (*FSM, *inmemory.Store, *model.Session) {
	t.Helper()
	snap, err := f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Release()
	sink := &sessionClockSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatal(err)
	}
	store := inmemory.New()
	restored := NewFSM(store, nil)
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return restored, store, got
}

func TestFSMSessionClockExplicitReplaySnapshot(t *testing.T) {
	epoch := time.Unix(0, 0).UTC()
	cases := []struct {
		name                               string
		at                                 time.Time
		revoke, expiryEpoch, creationEpoch bool
	}{
		{name: "rotate-current", at: time.Date(2026, 1, 1, 12, 30, 0, 123, time.UTC)},
		{name: "rotate-epoch", at: epoch},
		{name: "rotate-negative", at: time.Unix(0, -1)},
		{name: "rotate-epoch-expiry", at: epoch, expiryEpoch: true},
		{name: "rotate-epoch-creation", at: epoch, creationEpoch: true},
		{name: "revoke-current", at: time.Date(2026, 1, 1, 12, 30, 0, 123, time.UTC), revoke: true},
		{name: "revoke-epoch", at: epoch, revoke: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := clockSession(t)
			if tc.creationEpoch {
				original.SetCreatedAt(epoch)
			}
			wantExpiry := original.ExpiresAt()
			wantHash := original.RefreshHash()
			var mutation *raftv1.Op
			if tc.revoke {
				mutation = &raftv1.Op{Op: &raftv1.Op_SessionRevoke{SessionRevoke: &raftv1.SessionRevokeMsg{Id: original.ID(), RevokedAtNs: tc.at.UnixNano()}}}
			} else {
				wantHash = []byte("explicit-rotated")
				wantExpiry = wantExpiry.Add(time.Hour)
				if tc.expiryEpoch {
					wantExpiry = epoch
				}
				ns := tc.at.UnixNano()
				mutation = &raftv1.Op{Op: &raftv1.Op_SessionRotateRefresh{SessionRotateRefresh: &raftv1.SessionRotateRefreshMsg{Id: original.ID(), RefreshHash: wantHash, ExpiresAtNs: wantExpiry.UnixNano(), UpdatedAtNs: &ns}}}
			}
			data := clockCommand(t, mutation)
			assertSessionClockReplicas(t, original, mutation, tc.at)
			store := inmemory.New()
			f := NewFSM(store, nil)
			applyClockCommand(t, f, clockCommand(t, &raftv1.Op{Op: &raftv1.Op_SessionCreate{SessionCreate: wire.SessionToProto(original)}}), 1)
			applyClockCommand(t, f, data, 2)
			restored, restoredStore, got := restoreSessionClock(t, f, original.ID())
			if !got.CreatedAt().Equal(original.CreatedAt()) || !got.UpdatedAt().Equal(tc.at) || !got.ExpiresAt().Equal(wantExpiry) || !bytes.Equal(got.RefreshHash(), wantHash) || got.Revoked() != tc.revoke {
				t.Fatalf("command/snapshot state changed: created=%v updated=%v expires=%v revoked=%v", got.CreatedAt(), got.UpdatedAt(), got.ExpiresAt(), got.Revoked())
			}
			if tc.revoke && !got.RevokedAt().Equal(tc.at) {
				t.Fatalf("revocation changed: %v", got.RevokedAt())
			}
			before := wire.SessionToProto(got)
			// Duplicate application after recovery must be an idempotent full-state no-op.
			applyClockCommand(t, restored, data, 3)
			got, err := restoredStore.GetSession(context.Background(), original.ID())
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(before, wire.SessionToProto(got)) {
				t.Fatal("duplicate command changed restored state")
			}
		})
	}
}

func TestFSMSessionClockLegacyReplaySnapshot(t *testing.T) {
	for _, version := range []byte{2, commandVersion} {
		for _, unset := range []bool{false, true} {
			name := map[byte]string{2: "version2", commandVersion: "current"}[version] + map[bool]string{false: "-previous", true: "-unset"}[unset]
			t.Run(name, func(t *testing.T) {
				original := clockSession(t)
				if unset {
					original.SetUpdatedAt(time.Time{})
				}
				store := inmemory.New()
				f := NewFSM(store, nil)
				seed := wire.SessionToProto(original)
				// Old writers carried scalar timestamp values only, without presence flags.
				seed.CreatedAtPresent = false
				seed.UpdatedAtPresent = false
				seed.ExpiresAtPresent = false
				seed.RevokedAtPresent = false
				create := clockCommand(t, &raftv1.Op{Op: &raftv1.Op_SessionCreate{SessionCreate: seed}})
				create[len(commandMagic)] = version
				applyClockCommand(t, f, create, 1)
				mutation := clockCommand(t, &raftv1.Op{Op: &raftv1.Op_SessionRotateRefresh{SessionRotateRefresh: &raftv1.SessionRotateRefreshMsg{Id: original.ID(), RefreshHash: []byte("legacy-rotation"), ExpiresAtNs: original.ExpiresAt().Add(time.Hour).UnixNano()}}})
				mutation[len(commandMagic)] = version
				applyClockCommand(t, f, mutation, 2)
				restored, restoredStore, got := restoreSessionClock(t, f, original.ID())
				if !got.UpdatedAt().Equal(original.UpdatedAt()) {
					t.Fatalf("legacy replay invented a timestamp: %v != %v", got.UpdatedAt(), original.UpdatedAt())
				}
				before := wire.SessionToProto(got)
				applyClockCommand(t, restored, mutation, 3)
				got, err := restoredStore.GetSession(context.Background(), original.ID())
				if err != nil {
					t.Fatal(err)
				}
				if !proto.Equal(before, wire.SessionToProto(got)) {
					t.Fatal("legacy duplicate changed snapshot state")
				}
			})
		}
	}
}

func TestSessionTxReadOwnMutations(t *testing.T) {
	ctx := context.Background()
	inner := inmemory.New()
	original := clockSession(t)
	if err := inner.CreateSession(ctx, original); err != nil {
		t.Fatal(err)
	}
	view := newTxView(inner)
	at := time.Unix(0, 0).UTC()
	newExpiry := original.ExpiresAt().Add(time.Hour)
	if err := view.RotateRefreshAt(ctx, original.ID(), []byte("overlay-rotation"), newExpiry, at); err != nil {
		t.Fatal(err)
	}
	rotated, err := view.GetSession(ctx, original.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rotated.RefreshHash(), []byte("overlay-rotation")) || !rotated.ExpiresAt().Equal(newExpiry) || !rotated.UpdatedAt().Equal(at) {
		t.Fatal("rotation missing from transaction read")
	}
	if view.ops[0].GetSessionRotateRefresh().UpdatedAtNs == nil || view.ops[0].GetSessionRotateRefresh().GetUpdatedAtNs() != 0 {
		t.Fatal("explicit epoch omitted from recorded command")
	}
	rotated.SetUpdatedAt(at.Add(time.Hour))
	reread, err := view.GetSession(ctx, original.ID())
	if err != nil || !reread.UpdatedAt().Equal(at) {
		t.Fatal("returned session aliases pending overlay")
	}
	revokedAt := original.CreatedAt().Add(time.Hour)
	if err := view.RevokeSession(ctx, original.ID(), revokedAt); err != nil {
		t.Fatal(err)
	}
	revoked, err := view.GetSession(ctx, original.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !revoked.Revoked() || !revoked.RevokedAt().Equal(revokedAt) || !revoked.UpdatedAt().Equal(revokedAt) || !bytes.Equal(revoked.RefreshHash(), []byte("overlay-rotation")) {
		t.Fatal("revocation missing from transaction read")
	}
	if err := view.RevokeSession(ctx, original.ID(), revokedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	again, err := view.GetSession(ctx, original.ID())
	if err != nil || !again.UpdatedAt().Equal(revokedAt) || !again.RevokedAt().Equal(revokedAt) {
		t.Fatal("repeated revocation overwrote first timestamp")
	}
	stored, err := inner.GetSession(ctx, original.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(wire.SessionToProto(original), wire.SessionToProto(stored)) {
		t.Fatal("recording view published changes before commit")
	}
	// The exact recorded operations produce the same state as the read overlay.
	f := NewFSM(inner, nil)
	data, err := encodeCommand(view.ops)
	if err != nil {
		t.Fatal(err)
	}
	applyClockCommand(t, f, data, 1)
	stored, err = inner.GetSession(ctx, original.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(wire.SessionToProto(again), wire.SessionToProto(stored)) {
		t.Fatal("overlay and authoritative committed state differ")
	}
}

func TestSessionTxRejectedMutationsRecordNoOperations(t *testing.T) {
	ctx := context.Background()
	inner := inmemory.New()
	original := clockSession(t)
	if err := inner.CreateSession(ctx, original); err != nil {
		t.Fatal(err)
	}
	view := newTxView(inner)
	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"missing-rotation", func() error { return view.RotateRefresh(ctx, "missing", []byte("hash"), original.ExpiresAt()) }, storage.ErrNotFound},
		{"invalid-hash", func() error { return view.RotateRefresh(ctx, original.ID(), nil, original.ExpiresAt()) }, storage.ErrInvalidArgument},
		{"invalid-expiry", func() error { return view.RotateRefresh(ctx, original.ID(), []byte("hash"), time.Time{}) }, storage.ErrInvalidArgument},
		{"missing-revoke", func() error { return view.RevokeSession(ctx, "missing", original.CreatedAt()) }, storage.ErrNotFound},
		{"invalid-revoke-time", func() error { return view.RevokeSession(ctx, original.ID(), time.Time{}) }, storage.ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if len(view.ops) != 0 || len(view.sessions) != 0 {
				t.Fatal("rejected mutation recorded an operation or overlay")
			}
		})
	}
}

func TestSessionTxCallbackFailureDoesNotPublish(t *testing.T) {
	ctx := context.Background()
	p, _, hub := openRestartProfile(t, t.TempDir())
	defer hub.Close()
	defer func() { _ = p.Shutdown() }()
	original := clockSession(t)
	if err := p.Store().CreateSession(ctx, original); err != nil {
		t.Fatal(err)
	}
	reject := errors.New("reject callback after local mutations")
	err := p.Store().WithTx(ctx, func(tx storage.Storage) error {
		clocked, ok := tx.(storage.ClockedSessionRotation)
		if !ok {
			t.Fatal("recording transaction lacks explicit clock support")
		}
		if err := clocked.RotateRefreshAt(ctx, original.ID(), []byte("rejected-hash"), original.ExpiresAt().Add(time.Hour), original.CreatedAt().Add(time.Hour)); err != nil {
			return err
		}
		rotated, err := tx.GetSession(ctx, original.ID())
		if err != nil {
			return err
		}
		if !bytes.Equal(rotated.RefreshHash(), []byte("rejected-hash")) {
			t.Fatal("callback did not read its own rotation")
		}
		if err := tx.RevokeSession(ctx, original.ID(), original.CreatedAt().Add(2*time.Hour)); err != nil {
			return err
		}
		revoked, err := tx.GetSession(ctx, original.ID())
		if err != nil {
			return err
		}
		if !revoked.Revoked() {
			t.Fatal("callback did not read its own revocation")
		}
		return reject
	})
	if !errors.Is(err, reject) {
		t.Fatalf("callback error lost: %v", err)
	}
	stored, err := p.Store().GetSession(ctx, original.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(wire.SessionToProto(original), wire.SessionToProto(stored)) {
		t.Fatal("rejected callback published session mutations")
	}
}
