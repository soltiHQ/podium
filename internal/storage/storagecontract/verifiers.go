package storagecontract

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// verifiers checks the verifier rows: the round trip, replacement, the rows a
// store refuses, deletion, the lookup by credential and the deletion of a
// credential's verifiers.
func verifiers(t *testing.T, newStore storeFunc) {
	t.Run("round trip", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := fullVerifier(t, "ver-a", "cred-a", 0, minute(0))
		must(t, s.UpsertVerifier(ctx, want))

		got, err := s.GetVerifier(ctx, want.ID())
		gotRow(t, "GetVerifier", got, err, want, equalVerifier)
		got, err = s.GetVerifierByCredential(ctx, want.CredentialID())
		gotRow(t, "GetVerifierByCredential", got, err, want, equalVerifier)
	})

	t.Run("upsert replaces", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		must(t, s.UpsertVerifier(ctx, fullVerifier(t, "ver-a", "cred-a", 0, minute(0))))

		// Every field changes, the credential too: the lookup finds the row by its
		// new credential alone.
		want := fullVerifier(t, "ver-a", "cred-b", 1, minute(5))
		must(t, s.UpsertVerifier(ctx, want))
		got, err := s.GetVerifier(ctx, want.ID())
		gotRow(t, "GetVerifier", got, err, want, equalVerifier)
		got, err = s.GetVerifierByCredential(ctx, "cred-b")
		gotRow(t, "GetVerifierByCredential of the new credential", got, err, want, equalVerifier)
		expect(t, outcome{"GetVerifierByCredential of the old credential", errOf(s.GetVerifierByCredential(ctx, "cred-a")), storage.ErrNotFound})
	})

	t.Run("invalid rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		noID := &model.Verifier{}
		noID.SetCreatedAt(minute(0))
		noCreation := fullVerifier(t, "ver-a", "cred-a", 0, minute(0))
		noCreation.SetCreatedAt(time.Time{})
		expect(t,
			outcome{"UpsertVerifier of nil", s.UpsertVerifier(ctx, nil), storage.ErrInvalidArgument},
			outcome{"UpsertVerifier of a row without an ID", s.UpsertVerifier(ctx, noID), storage.ErrInvalidArgument},
			outcome{"UpsertVerifier of a row without a creation time", s.UpsertVerifier(ctx, noCreation), storage.ErrInvalidArgument},
			outcome{"GetVerifier of the row without a creation time", errOf(s.GetVerifier(ctx, "ver-a")), storage.ErrNotFound},
			outcome{"GetVerifierByCredential of the row without a creation time", errOf(s.GetVerifierByCredential(ctx, "cred-a")), storage.ErrNotFound},
		)
	})

	t.Run("delete and missing rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		kept := fullVerifier(t, "ver-b", "cred-b", 0, minute(0))
		must(t, errors.Join(s.UpsertVerifier(ctx, fullVerifier(t, "ver-a", "cred-a", 0, minute(0))), s.UpsertVerifier(ctx, kept)))
		expect(t,
			outcome{"DeleteVerifier", s.DeleteVerifier(ctx, "ver-a"), nil},
			outcome{"GetVerifier of a deleted row", errOf(s.GetVerifier(ctx, "ver-a")), storage.ErrNotFound},
			outcome{"GetVerifierByCredential of a deleted row", errOf(s.GetVerifierByCredential(ctx, "cred-a")), storage.ErrNotFound},
			outcome{"DeleteVerifier of a deleted row", s.DeleteVerifier(ctx, "ver-a"), storage.ErrNotFound},
			outcome{"GetVerifier of an ID never written", errOf(s.GetVerifier(ctx, "ver-x")), storage.ErrNotFound},
			outcome{"DeleteVerifier of an ID never written", s.DeleteVerifier(ctx, "ver-x"), storage.ErrNotFound},
			outcome{"GetVerifier of an empty ID", errOf(s.GetVerifier(ctx, "")), storage.ErrInvalidArgument},
			outcome{"DeleteVerifier of an empty ID", s.DeleteVerifier(ctx, ""), storage.ErrInvalidArgument},
		)
		got, err := s.GetVerifier(ctx, kept.ID())
		gotRow(t, "GetVerifier of the row kept", got, err, kept, equalVerifier)
	})

	t.Run("lookup by credential", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// Credential IDs that differ from cred-a by a suffix or in case: an exact
		// comparison finds cred-a's row alone.
		want := fullVerifier(t, "ver-a", "cred-a", 0, minute(0))
		must(t, errors.Join(
			s.UpsertVerifier(ctx, want),
			s.UpsertVerifier(ctx, fullVerifier(t, "ver-b", "cred-ab", 0, minute(0))),
			s.UpsertVerifier(ctx, fullVerifier(t, "ver-c", "CRED-A", 0, minute(0))),
		))
		got, err := s.GetVerifierByCredential(ctx, "cred-a")
		gotRow(t, "GetVerifierByCredential", got, err, want, equalVerifier)
		expect(t,
			outcome{"GetVerifierByCredential of a prefix", errOf(s.GetVerifierByCredential(ctx, "cred-")), storage.ErrNotFound},
			outcome{"GetVerifierByCredential of a credential never written", errOf(s.GetVerifierByCredential(ctx, "cred-x")), storage.ErrNotFound},
			outcome{"GetVerifierByCredential of an empty credential ID", errOf(s.GetVerifierByCredential(ctx, "")), storage.ErrInvalidArgument},
		)
		// The credential is indexed, not unique: two rows of one credential are a fault.
		must(t, s.UpsertVerifier(ctx, fullVerifier(t, "ver-d", "cred-a", 1, minute(1))))
		expect(t, outcome{"GetVerifierByCredential of a credential two rows hold", errOf(s.GetVerifierByCredential(ctx, "cred-a")), storage.ErrInternal})
	})

	t.Run("delete by credential", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// Two verifiers of cred-a, and verifiers of credentials whose IDs differ
		// from cred-a by a suffix or in case.
		kept1, kept2 := fullVerifier(t, "ver-c", "cred-ab", 0, minute(0)), fullVerifier(t, "ver-d", "CRED-A", 0, minute(0))
		must(t, errors.Join(
			s.UpsertVerifier(ctx, fullVerifier(t, "ver-a", "cred-a", 0, minute(0))),
			s.UpsertVerifier(ctx, fullVerifier(t, "ver-b", "cred-a", 1, minute(1))),
			s.UpsertVerifier(ctx, kept1),
			s.UpsertVerifier(ctx, kept2),
		))
		expect(t,
			outcome{"DeleteVerifierByCredential", s.DeleteVerifierByCredential(ctx, "cred-a"), nil},
			outcome{"GetVerifier of ver-a", errOf(s.GetVerifier(ctx, "ver-a")), storage.ErrNotFound},
			outcome{"GetVerifier of ver-b", errOf(s.GetVerifier(ctx, "ver-b")), storage.ErrNotFound},
			outcome{"DeleteVerifierByCredential again", s.DeleteVerifierByCredential(ctx, "cred-a"), nil},
			outcome{"DeleteVerifierByCredential of a credential with no verifier", s.DeleteVerifierByCredential(ctx, "cred-x"), nil},
			outcome{"DeleteVerifierByCredential of an empty credential ID", s.DeleteVerifierByCredential(ctx, ""), storage.ErrInvalidArgument},
		)
		for _, want := range []*model.Verifier{kept1, kept2} {
			got, err := s.GetVerifier(ctx, want.ID())
			gotRow(t, "GetVerifier of a row kept", got, err, want, equalVerifier)
		}
	})
}

// fullVerifier is verifier id of credentialID with every other field set from
// v: with v 0, none to its zero or default value.
func fullVerifier(t *testing.T, id, credentialID string, v int, created time.Time) *model.Verifier {
	t.Helper()
	vf, err := model.NewVerifier(id, credentialID, []enum.Auth{enum.Password, enum.APIKey}[v%2])
	if err != nil {
		t.Fatal(err)
	}
	must(t, errors.Join(
		vf.DataSet("argon2id", fmt.Sprintf("digest-%s-%d", id, v)),
		vf.DataSet(fmt.Sprintf("params-%d", v), "m=65536,t=3,p=4"),
	))
	vf.SetCreatedAt(created)
	vf.SetUpdatedAt(created.Add(time.Duration(v+1) * time.Minute))
	return vf
}

// equalVerifier compares every accessor of a verifier read back with the
// verifier written.
func equalVerifier(t *testing.T, what string, got, want *model.Verifier) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil verifier, want %s", what, want.ID())
		return
	}
	f := fields{t, what}
	f.same("ID", got.ID(), want.ID())
	f.same("CredentialID", got.CredentialID(), want.CredentialID())
	f.same("AuthKind", got.AuthKind(), want.AuthKind())
	f.same("DataAll", got.DataAll(), want.DataAll())
	f.instant("CreatedAt", got.CreatedAt(), want.CreatedAt())
	f.instant("UpdatedAt", got.UpdatedAt(), want.UpdatedAt())
}
