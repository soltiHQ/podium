package durable

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
)

func TestSessionClockedRotationAndEpochRevocationPersist(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "sessions")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	epoch := time.Unix(0, 0).UTC()
	original, err := model.NewSession("s", "u", "c", enum.Password, []byte("old"), epoch.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	original.SetCreatedAt(epoch)
	if err := s.CreateSession(ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := s.RotateRefreshAt(ctx, "s", []byte("rotated"), epoch, epoch); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, "s", epoch); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	got, err := restored.GetSession(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt().Equal(epoch) || !got.UpdatedAt().Equal(epoch) || !got.ExpiresAt().Equal(epoch) || !got.Revoked() || !got.RevokedAt().Equal(epoch) || !bytes.Equal(got.RefreshHash(), []byte("rotated")) {
		t.Fatal("durable snapshot lost explicit epoch session timestamps or mutation")
	}
}
