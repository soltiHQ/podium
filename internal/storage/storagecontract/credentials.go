package storagecontract

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// credentials checks the credential rows: the round trip, replacement, the rows
// a store refuses, deletion, the lookup by user and auth kind and the listing
// of a user's credentials.
func credentials(t *testing.T, newStore storeFunc) {
	t.Run("round trip", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := fullCredential(t, "cred-a", "user-a", enum.Password, 0, minute(0))
		must(t, s.UpsertCredential(ctx, want))

		got, err := s.GetCredential(ctx, want.ID())
		gotRow(t, "GetCredential", got, err, want, equalCredential)
		got, err = s.GetCredentialByUserAndAuth(ctx, want.UserID(), want.AuthKind())
		gotRow(t, "GetCredentialByUserAndAuth", got, err, want, equalCredential)
		listed, err := s.ListCredentialsByUser(ctx, want.UserID())
		must(t, err)
		equalRows(t, "ListCredentialsByUser", listed, []*model.Credential{want}, equalCredential)
	})

	t.Run("upsert replaces", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		must(t, s.UpsertCredential(ctx, fullCredential(t, "cred-a", "user-a", enum.Password, 0, minute(0))))

		// Every field changes, the user and the auth kind too: the lookups find
		// the row by its new keys alone.
		want := fullCredential(t, "cred-a", "user-b", enum.APIKey, 1, minute(5))
		must(t, s.UpsertCredential(ctx, want))
		got, err := s.GetCredential(ctx, want.ID())
		gotRow(t, "GetCredential", got, err, want, equalCredential)
		got, err = s.GetCredentialByUserAndAuth(ctx, "user-b", enum.APIKey)
		gotRow(t, "GetCredentialByUserAndAuth of the new keys", got, err, want, equalCredential)
		expect(t, outcome{"GetCredentialByUserAndAuth of the old keys", errOf(s.GetCredentialByUserAndAuth(ctx, "user-a", enum.Password)), storage.ErrNotFound})
		listed, err := s.ListCredentialsByUser(ctx, "user-b")
		must(t, err)
		equalRows(t, "ListCredentialsByUser of the new user", listed, []*model.Credential{want}, equalCredential)
		listed, err = s.ListCredentialsByUser(ctx, "user-a")
		sameIDs(t, "ListCredentialsByUser of the old user", listed, err)
	})

	t.Run("invalid rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		noID := &model.Credential{}
		noID.SetCreatedAt(minute(0))
		noCreation := fullCredential(t, "cred-a", "user-a", enum.Password, 0, minute(0))
		noCreation.SetCreatedAt(time.Time{})
		expect(t,
			outcome{"UpsertCredential of nil", s.UpsertCredential(ctx, nil), storage.ErrInvalidArgument},
			outcome{"UpsertCredential of a row without an ID", s.UpsertCredential(ctx, noID), storage.ErrInvalidArgument},
			outcome{"UpsertCredential of a row without a creation time", s.UpsertCredential(ctx, noCreation), storage.ErrInvalidArgument},
			outcome{"GetCredential of the row without a creation time", errOf(s.GetCredential(ctx, "cred-a")), storage.ErrNotFound},
		)
		listed, err := s.ListCredentialsByUser(ctx, "user-a")
		sameIDs(t, "ListCredentialsByUser after the refused writes", listed, err)
	})

	t.Run("delete and missing rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		kept := fullCredential(t, "cred-b", "user-a", enum.APIKey, 0, minute(0))
		must(t, errors.Join(s.UpsertCredential(ctx, fullCredential(t, "cred-a", "user-a", enum.Password, 0, minute(0))), s.UpsertCredential(ctx, kept)))
		expect(t,
			outcome{"DeleteCredential", s.DeleteCredential(ctx, "cred-a"), nil},
			outcome{"GetCredential of a deleted row", errOf(s.GetCredential(ctx, "cred-a")), storage.ErrNotFound},
			outcome{"GetCredentialByUserAndAuth of a deleted row", errOf(s.GetCredentialByUserAndAuth(ctx, "user-a", enum.Password)), storage.ErrNotFound},
			outcome{"DeleteCredential of a deleted row", s.DeleteCredential(ctx, "cred-a"), storage.ErrNotFound},
			outcome{"GetCredential of an ID never written", errOf(s.GetCredential(ctx, "cred-x")), storage.ErrNotFound},
			outcome{"DeleteCredential of an ID never written", s.DeleteCredential(ctx, "cred-x"), storage.ErrNotFound},
			outcome{"GetCredential of an empty ID", errOf(s.GetCredential(ctx, "")), storage.ErrInvalidArgument},
			outcome{"DeleteCredential of an empty ID", s.DeleteCredential(ctx, ""), storage.ErrInvalidArgument},
		)
		got, err := s.GetCredential(ctx, kept.ID())
		gotRow(t, "GetCredential of the row kept", got, err, kept, equalCredential)
	})

	t.Run("lookup by user and auth kind", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// User IDs that differ from user-a by a suffix or in case: an exact
		// comparison finds user-a's rows alone.
		password := fullCredential(t, "cred-a", "user-a", enum.Password, 0, minute(0))
		apiKey := fullCredential(t, "cred-b", "user-a", enum.APIKey, 1, minute(0))
		must(t, errors.Join(
			s.UpsertCredential(ctx, password),
			s.UpsertCredential(ctx, apiKey),
			s.UpsertCredential(ctx, fullCredential(t, "cred-c", "user-ab", enum.Password, 0, minute(0))),
			s.UpsertCredential(ctx, fullCredential(t, "cred-d", "USER-A", enum.Password, 0, minute(0))),
		))
		got, err := s.GetCredentialByUserAndAuth(ctx, "user-a", enum.Password)
		gotRow(t, "GetCredentialByUserAndAuth of the password", got, err, password, equalCredential)
		got, err = s.GetCredentialByUserAndAuth(ctx, "user-a", enum.APIKey)
		gotRow(t, "GetCredentialByUserAndAuth of the API key", got, err, apiKey, equalCredential)
		expect(t,
			outcome{"GetCredentialByUserAndAuth of an auth kind in another case", errOf(s.GetCredentialByUserAndAuth(ctx, "user-a", "PASSWORD")), storage.ErrNotFound},
			outcome{"GetCredentialByUserAndAuth of an auth kind the user has none of", errOf(s.GetCredentialByUserAndAuth(ctx, "user-a", "token")), storage.ErrNotFound},
			outcome{"GetCredentialByUserAndAuth of a prefix of a user ID", errOf(s.GetCredentialByUserAndAuth(ctx, "user-", enum.Password)), storage.ErrNotFound},
			outcome{"GetCredentialByUserAndAuth of a user ID never written", errOf(s.GetCredentialByUserAndAuth(ctx, "user-x", enum.Password)), storage.ErrNotFound},
			outcome{"GetCredentialByUserAndAuth of an empty user ID", errOf(s.GetCredentialByUserAndAuth(ctx, "", enum.Password)), storage.ErrInvalidArgument},
		)
		// The user and auth kind are indexed, not unique: two rows of them are a fault.
		must(t, s.UpsertCredential(ctx, fullCredential(t, "cred-e", "user-a", enum.Password, 1, minute(1))))
		expect(t, outcome{"GetCredentialByUserAndAuth of keys two rows hold", errOf(s.GetCredentialByUserAndAuth(ctx, "user-a", enum.Password)), storage.ErrInternal})
	})

	t.Run("listing by user", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// user-a's rows, two of them created at one time, and rows of users whose
		// IDs differ from user-a's by a suffix or in case.
		for _, r := range []struct {
			id, user string
			created  time.Time
		}{
			{"cred-c", "user-a", minute(1)},
			{"cred-b", "user-a", minute(2)},
			{"cred-a", "user-a", minute(1)},
			{"cred-d", "user-ab", minute(3)},
			{"cred-e", "USER-A", minute(3)},
		} {
			must(t, s.UpsertCredential(ctx, fullCredential(t, r.id, r.user, enum.Password, 0, r.created)))
		}
		listed, err := s.ListCredentialsByUser(ctx, "user-a")
		must(t, err)
		if got, want := idsOf(listed), []string{"cred-b", "cred-a", "cred-c"}; !slices.Equal(got, want) {
			t.Errorf("ListCredentialsByUser: rows %v, want %v in the listing order", got, want)
		}
		// The spec is silent on a user with no credential: the in-memory store,
		// which drains a listing, returns nil.
		none, err := s.ListCredentialsByUser(ctx, "user-x")
		must(t, err)
		if none != nil {
			t.Errorf("ListCredentialsByUser of a user with no credential: %v, want nil", idsOf(none))
		}
		expect(t, outcome{"ListCredentialsByUser of an empty user ID", errOf(s.ListCredentialsByUser(ctx, "")), storage.ErrInvalidArgument})
	})
}

// fullCredential is credential id of userID and auth with every other field set
// from v: with v 0, none to its zero or default value.
func fullCredential(t *testing.T, id, userID string, auth enum.Auth, v int, created time.Time) *model.Credential {
	t.Helper()
	c, err := model.NewCredential(id, userID, auth)
	if err != nil {
		t.Fatal(err)
	}
	must(t, errors.Join(
		c.SetSecret("hash", fmt.Sprintf("hash-%s-%d", id, v)),
		c.SetSecret(fmt.Sprintf("salt-%d", v), fmt.Sprintf("salt-%s", id)),
	))
	c.SetCreatedAt(created)
	c.SetUpdatedAt(created.Add(time.Duration(v+1) * time.Minute))
	return c
}

// equalCredential compares every accessor of a credential read back with the
// credential written.
func equalCredential(t *testing.T, what string, got, want *model.Credential) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil credential, want %s", what, want.ID())
		return
	}
	f := fields{t, what}
	f.same("ID", got.ID(), want.ID())
	f.same("UserID", got.UserID(), want.UserID())
	f.same("AuthKind", got.AuthKind(), want.AuthKind())
	f.same("SecretsAll", got.SecretsAll(), want.SecretsAll())
	f.instant("CreatedAt", got.CreatedAt(), want.CreatedAt())
	f.instant("UpdatedAt", got.UpdatedAt(), want.UpdatedAt())
}
