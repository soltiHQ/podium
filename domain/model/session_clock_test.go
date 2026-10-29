package model

import (
	"bytes"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
)

func TestSessionAtomicRotationUsesExplicitClock(t *testing.T) {
	old := time.Date(2025, 1, 1, 0, 0, 0, 71, time.UTC)
	expiry := old.Add(time.Hour)
	s, err := NewSession("s", "u", "c", enum.Password, []byte("old"), expiry)
	if err != nil {
		t.Fatal(err)
	}
	s.SetUpdatedAt(old)
	epoch := time.Unix(0, 0)
	hash := []byte("new")
	if err := s.RotateRefreshAt(hash, expiry.Add(time.Hour), epoch); err != nil {
		t.Fatal(err)
	}
	hash[0] = 'x'
	if !s.UpdatedAt().Equal(epoch) || !bytes.Equal(s.RefreshHash(), []byte("new")) {
		t.Fatal("timestamp or owned hash changed")
	}
	if err := s.RotateRefreshAt([]byte("new"), expiry.Add(time.Hour), old); err != nil {
		t.Fatal(err)
	}
	if !s.UpdatedAt().Equal(epoch) {
		t.Fatal("idempotent rotation changed timestamp")
	}
	if err := s.RotateRefreshAt([]byte("legacy"), expiry, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !s.UpdatedAt().Equal(epoch) {
		t.Fatal("legacy omitted clock replaced stored timestamp")
	}
}

func TestSessionAtomicRotationValidatesBeforeMutation(t *testing.T) {
	at := time.Date(2025, 1, 1, 0, 0, 0, 91, time.UTC)
	s, err := NewSession("s", "u", "c", enum.Password, []byte("old"), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s.SetUpdatedAt(at)
	for _, invalid := range []struct {
		hash   []byte
		expiry time.Time
	}{{nil, at}, {[]byte("new"), time.Time{}}} {
		if err := s.RotateRefreshAt(invalid.hash, invalid.expiry, at.Add(time.Hour)); err == nil {
			t.Fatal("invalid rotation accepted")
		}
		if !bytes.Equal(s.RefreshHash(), []byte("old")) || !s.ExpiresAt().Equal(at.Add(time.Hour)) || !s.UpdatedAt().Equal(at) {
			t.Fatal("invalid rotation partially mutated session")
		}
	}
}

func TestSessionRevokeUsesFirstAuthoritativeTime(t *testing.T) {
	at := time.Unix(0, 0)
	s, err := NewSession("s", "u", "c", enum.Password, []byte("old"), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(at); err != nil {
		t.Fatal(err)
	}
	if !s.Revoked() || !s.RevokedAt().Equal(at) || !s.UpdatedAt().Equal(at) {
		t.Fatal("revocation ignored authoritative epoch")
	}
	if err := s.Revoke(at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !s.RevokedAt().Equal(at) || !s.UpdatedAt().Equal(at) {
		t.Fatal("duplicate revocation changed timestamp")
	}
}
