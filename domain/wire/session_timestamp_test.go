package wire_test

import (
	"testing"
	"time"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"google.golang.org/protobuf/proto"
)

func TestSessionUpdatedAtPresenceRoundTrip(t *testing.T) {
	for _, row := range []struct {
		name string
		at   time.Time
	}{
		{"unset", time.Time{}},
		{"epoch", time.Unix(0, 0)},
		{"before epoch", time.Unix(-1, 123)},
		{"current", time.Unix(1790896000, 123456789)},
	} {
		t.Run(row.name, func(t *testing.T) {
			session, err := model.NewSession("session", "user", "credential", enum.Auth("local"), []byte("hash"), time.Unix(1790996000, 0))
			if err != nil {
				t.Fatal(err)
			}
			session.SetUpdatedAt(row.at)
			encoded := wire.SessionToProto(session)
			if encoded.GetUpdatedAtPresent() == row.at.IsZero() {
				t.Fatal("timestamp presence changed")
			}
			data, err := proto.Marshal(encoded)
			if err != nil {
				t.Fatal(err)
			}
			var decoded raftv1.SessionMsg
			if err := proto.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			restored, err := wire.SessionFromProto(&decoded)
			if err != nil {
				t.Fatal(err)
			}
			if !restored.UpdatedAt().Equal(row.at) || restored.UpdatedAt().IsZero() != row.at.IsZero() {
				t.Fatalf("updatedAt = %v, want %v", restored.UpdatedAt(), row.at)
			}
		})
	}
}

func TestSessionLegacyUpdatedAtWithoutPresence(t *testing.T) {
	for _, ns := range []int64{0, -1, 1790896000123456789} {
		session, err := wire.SessionFromProto(&raftv1.SessionMsg{Id: "session", UserId: "user", CredentialId: "credential", Auth: "local", RefreshHash: []byte("hash"), ExpiresAtNs: 1790996000000000000, UpdatedAtNs: ns})
		if err != nil {
			t.Fatal(err)
		}
		if ns == 0 {
			if !session.UpdatedAt().IsZero() {
				t.Fatal("legacy unset timestamp became epoch")
			}
		} else if session.UpdatedAt().UnixNano() != ns {
			t.Fatal("legacy nonzero timestamp changed")
		}
	}
}

func TestSessionEpochCreationExpiryAndRevocationSurviveWire(t *testing.T) {
	epoch := time.Unix(0, 0)
	session, err := model.NewSession("session", "user", "credential", enum.Auth("local"), []byte("hash"), epoch)
	if err != nil {
		t.Fatal(err)
	}
	session.SetCreatedAt(epoch)
	session.SetUpdatedAt(epoch)
	if err := session.Revoke(epoch); err != nil {
		t.Fatal(err)
	}
	encoded := wire.SessionToProto(session)
	if !encoded.GetRevokedAtPresent() || !encoded.GetExpiresAtPresent() || !encoded.GetCreatedAtPresent() {
		t.Fatal("epoch timestamp presence lost")
	}
	data, err := proto.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var decoded raftv1.SessionMsg
	if err := proto.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	restored, err := wire.SessionFromProto(&decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Revoked() || !restored.RevokedAt().Equal(epoch) {
		t.Fatal("revocation disappeared after wire restore")
	}
	if !restored.CreatedAt().Equal(epoch) || !restored.ExpiresAt().Equal(epoch) {
		t.Fatal("creation or expiration epoch changed")
	}
}

func TestSessionLegacyUnsetRevocationStaysUnset(t *testing.T) {
	session, err := wire.SessionFromProto(&raftv1.SessionMsg{Id: "session", UserId: "user", CredentialId: "credential", Auth: "local", RefreshHash: []byte("hash"), ExpiresAtNs: 1790996000000000000})
	if err != nil {
		t.Fatal(err)
	}
	if session.Revoked() || !session.CreatedAt().IsZero() || !session.UpdatedAt().IsZero() {
		t.Fatal("legacy unset timestamp acquired presence")
	}
}
