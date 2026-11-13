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

// sessions checks the session rows, which a store creates once and then rotates
// and revokes: the round trip of a full and of a minimal row, the rows a store
// refuses, deletion, rotation, revocation and the listing, beyond a page too,
// and deletion of a user's sessions.
func sessions(t *testing.T, newStore storeFunc) {
	t.Run("round trip", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := fullSession(t, "sess-a", "user-a", "cred-a", 0, minute(0))
		must(t, s.CreateSession(ctx, want))

		got, err := s.GetSession(ctx, want.ID())
		gotRow(t, "GetSession", got, err, want, equalSession)
		listed, err := s.ListSessionsByUser(ctx, want.UserID())
		must(t, err)
		equalRows(t, "ListSessionsByUser", listed, []*model.Session{want}, equalSession)
	})

	t.Run("round trip of a minimal row", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := minimalSession(t, "sess-a", "user-a", "cred-a", minute(0))
		must(t, s.CreateSession(ctx, want))

		got, err := s.GetSession(ctx, want.ID())
		gotRow(t, "GetSession", got, err, want, equalSession)
		listed, err := s.ListSessionsByUser(ctx, want.UserID())
		must(t, err)
		equalRows(t, "ListSessionsByUser", listed, []*model.Session{want}, equalSession)
	})

	t.Run("create of an existing ID", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := fullSession(t, "sess-a", "user-a", "cred-a", 0, minute(0))
		must(t, s.CreateSession(ctx, want))
		expect(t, outcome{"CreateSession of an existing ID", s.CreateSession(ctx, fullSession(t, "sess-a", "user-b", "cred-b", 1, minute(5))), storage.ErrAlreadyExists})
		got, err := s.GetSession(ctx, want.ID())
		gotRow(t, "GetSession", got, err, want, equalSession)
	})

	t.Run("invalid rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		noID := &model.Session{}
		noID.SetCreatedAt(minute(0))
		noCreation := fullSession(t, "sess-a", "user-a", "cred-a", 0, minute(0))
		noCreation.SetCreatedAt(time.Time{})
		expect(t,
			outcome{"CreateSession of nil", s.CreateSession(ctx, nil), storage.ErrInvalidArgument},
			outcome{"CreateSession of a row without an ID", s.CreateSession(ctx, noID), storage.ErrInvalidArgument},
			outcome{"CreateSession of a row without a creation time", s.CreateSession(ctx, noCreation), storage.ErrInvalidArgument},
			outcome{"GetSession of the row without a creation time", errOf(s.GetSession(ctx, "sess-a")), storage.ErrNotFound},
		)
		listed, err := s.ListSessionsByUser(ctx, "user-a")
		sameIDs(t, "ListSessionsByUser after the refused writes", listed, err)
	})

	t.Run("delete and missing rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		kept := fullSession(t, "sess-b", "user-a", "cred-a", 0, minute(0))
		must(t, errors.Join(s.CreateSession(ctx, fullSession(t, "sess-a", "user-a", "cred-a", 0, minute(0))), s.CreateSession(ctx, kept)))
		expect(t,
			outcome{"DeleteSession", s.DeleteSession(ctx, "sess-a"), nil},
			outcome{"GetSession of a deleted row", errOf(s.GetSession(ctx, "sess-a")), storage.ErrNotFound},
			outcome{"DeleteSession of a deleted row", s.DeleteSession(ctx, "sess-a"), storage.ErrNotFound},
			outcome{"RotateRefresh of a deleted row", s.RotateRefresh(ctx, "sess-a", []byte("rotated"), minute(60)), storage.ErrNotFound},
			outcome{"RevokeSession of a deleted row", s.RevokeSession(ctx, "sess-a", minute(30)), storage.ErrNotFound},
			outcome{"GetSession of an ID never written", errOf(s.GetSession(ctx, "sess-x")), storage.ErrNotFound},
			outcome{"DeleteSession of an ID never written", s.DeleteSession(ctx, "sess-x"), storage.ErrNotFound},
			outcome{"GetSession of an empty ID", errOf(s.GetSession(ctx, "")), storage.ErrInvalidArgument},
			outcome{"DeleteSession of an empty ID", s.DeleteSession(ctx, ""), storage.ErrInvalidArgument},
		)
		got, err := s.GetSession(ctx, kept.ID())
		gotRow(t, "GetSession of the row kept", got, err, kept, equalSession)
	})

	t.Run("rotate refresh", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		session := liveSession(t, "sess-a", "user-a", "cred-a", 0, minute(0))
		must(t, s.CreateSession(ctx, session))
		expect(t,
			outcome{"RotateRefresh of an empty ID", s.RotateRefresh(ctx, "", []byte("rotated"), minute(60)), storage.ErrInvalidArgument},
			outcome{"RotateRefresh to an empty hash", s.RotateRefresh(ctx, session.ID(), nil, minute(60)), storage.ErrInvalidArgument},
			outcome{"RotateRefresh to a zero expiry", s.RotateRefresh(ctx, session.ID(), []byte("rotated"), time.Time{}), storage.ErrInvalidArgument},
			// The same hash and expiry change nothing, the modification time included.
			outcome{"RotateRefresh to the same hash and expiry", s.RotateRefresh(ctx, session.ID(), session.RefreshHash(), session.ExpiresAt()), nil},
		)
		got, err := s.GetSession(ctx, session.ID())
		gotRow(t, "GetSession after the rotations that change nothing", got, err, session, equalSession)

		// A rotation replaces the hash and the expiry, and stamps the time of the call.
		before := time.Now()
		must(t, s.RotateRefresh(ctx, session.ID(), []byte("rotated"), minute(60)))
		after := time.Now()
		got, err = s.GetSession(ctx, session.ID())
		must(t, err)
		if got.UpdatedAt().Before(before) || got.UpdatedAt().After(after) {
			t.Errorf("RotateRefresh: UpdatedAt = %v, want the time of the call, %v to %v", got.UpdatedAt(), before, after)
		}
		want := session.Clone()
		must(t, want.RotateRefreshAt([]byte("rotated"), minute(60), got.UpdatedAt()))
		equalSession(t, "GetSession after RotateRefresh", got, want)
	})

	t.Run("rotate refresh at a time", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		clocked, ok := s.(storage.ClockedSessionRotation)
		if !ok {
			t.Fatal("the store does not implement storage.ClockedSessionRotation")
		}
		session := liveSession(t, "sess-a", "user-a", "cred-a", 0, minute(0))
		must(t, s.CreateSession(ctx, session))
		want := session.Clone()
		for _, step := range []struct {
			name      string
			hash      string
			expiresAt time.Time
			updatedAt time.Time
		}{
			// A zero time keeps the stored one, which is not the Unix epoch, so that
			// a store which writes a zero time as the epoch fails.
			{"at a zero time", "first", minute(60), time.Time{}},
			// The time given is the modification time, the Unix epoch included.
			{"at the Unix epoch", "second", minute(120), time.Unix(0, 0)},
			// The same hash and expiry change nothing, whatever the time.
			{"to the same hash and expiry", "second", minute(120), minute(500)},
		} {
			must(t, clocked.RotateRefreshAt(ctx, session.ID(), []byte(step.hash), step.expiresAt, step.updatedAt))
			must(t, want.RotateRefreshAt([]byte(step.hash), step.expiresAt, step.updatedAt))
			got, err := s.GetSession(ctx, session.ID())
			gotRow(t, "GetSession after RotateRefreshAt "+step.name, got, err, want, equalSession)
		}
		expect(t,
			outcome{"RotateRefreshAt of an empty ID", clocked.RotateRefreshAt(ctx, "", []byte("third"), minute(60), minute(1)), storage.ErrInvalidArgument},
			outcome{"RotateRefreshAt to an empty hash", clocked.RotateRefreshAt(ctx, session.ID(), nil, minute(60), minute(1)), storage.ErrInvalidArgument},
			outcome{"RotateRefreshAt to a zero expiry", clocked.RotateRefreshAt(ctx, session.ID(), []byte("third"), time.Time{}, minute(1)), storage.ErrInvalidArgument},
			outcome{"RotateRefreshAt of an ID never written", clocked.RotateRefreshAt(ctx, "sess-x", []byte("third"), minute(60), minute(1)), storage.ErrNotFound},
		)
	})

	t.Run("revoke", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		session := liveSession(t, "sess-a", "user-a", "cred-a", 0, minute(0))
		must(t, s.CreateSession(ctx, session))
		expect(t,
			outcome{"RevokeSession of an empty ID", s.RevokeSession(ctx, "", minute(10)), storage.ErrInvalidArgument},
			outcome{"RevokeSession at a zero time", s.RevokeSession(ctx, session.ID(), time.Time{}), storage.ErrInvalidArgument},
			outcome{"RevokeSession of an ID never written", s.RevokeSession(ctx, "sess-x", minute(10)), storage.ErrNotFound},
			outcome{"RevokeSession", s.RevokeSession(ctx, session.ID(), minute(10)), nil},
			// The first revocation wins: a later one changes nothing.
			outcome{"RevokeSession again", s.RevokeSession(ctx, session.ID(), minute(20)), nil},
		)
		want := session.Clone()
		must(t, want.Revoke(minute(10)))
		got, err := s.GetSession(ctx, session.ID())
		gotRow(t, "GetSession after the revocations", got, err, want, equalSession)
	})

	t.Run("listing by user", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		writeUserSessions(t, s)
		listed, err := s.ListSessionsByUser(ctx, "user-a")
		must(t, err)
		if got, want := idsOf(listed), []string{"sess-b", "sess-a", "sess-c"}; !slices.Equal(got, want) {
			t.Errorf("ListSessionsByUser: rows %v, want %v in the listing order", got, want)
		}
		// The spec is silent on a user with no session: the in-memory store, which
		// drains a listing, returns nil.
		none, err := s.ListSessionsByUser(ctx, "user-x")
		must(t, err)
		if none != nil {
			t.Errorf("ListSessionsByUser of a user with no session: %v, want nil", idsOf(none))
		}
		expect(t, outcome{"ListSessionsByUser of an empty user ID", errOf(s.ListSessionsByUser(ctx, "")), storage.ErrInvalidArgument})
	})

	t.Run("listing by user beyond a page", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// More sessions of one user than the largest page holds, written in one
		// transaction: the listing is not a page, it returns them all, so a store
		// that answers with one page, of the default size or the largest, fails.
		var rows []written
		must(t, s.WithTx(ctx, func(tx storage.Storage) error {
			for i := range storage.MaxListLimit + 1 {
				session := fullSession(t, fmt.Sprintf("sess-%03d", i), "user-a", "cred-a", 0, minute(i))
				if err := tx.CreateSession(ctx, session); err != nil {
					return err
				}
				rows = append(rows, written{session.ID(), session.CreatedAt()})
			}
			return nil
		}))
		got, err := s.ListSessionsByUser(ctx, "user-a")
		must(t, err)
		if ids, want := idsOf(got), listed(rows); !slices.Equal(ids, want) {
			t.Errorf("ListSessionsByUser: %d rows, want all %d in the listing order", len(ids), len(want))
		}
	})

	t.Run("delete by user", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		writeUserSessions(t, s)
		expect(t,
			outcome{"DeleteSessionsByUser", s.DeleteSessionsByUser(ctx, "user-a"), nil},
			outcome{"GetSession of sess-a", errOf(s.GetSession(ctx, "sess-a")), storage.ErrNotFound},
			outcome{"GetSession of sess-b", errOf(s.GetSession(ctx, "sess-b")), storage.ErrNotFound},
			outcome{"GetSession of sess-c", errOf(s.GetSession(ctx, "sess-c")), storage.ErrNotFound},
			outcome{"GetSession of sess-d, of user-ab", errOf(s.GetSession(ctx, "sess-d")), nil},
			outcome{"GetSession of sess-e, of USER-A", errOf(s.GetSession(ctx, "sess-e")), nil},
			outcome{"DeleteSessionsByUser again", s.DeleteSessionsByUser(ctx, "user-a"), nil},
			outcome{"DeleteSessionsByUser of a user with no session", s.DeleteSessionsByUser(ctx, "user-x"), nil},
			outcome{"DeleteSessionsByUser of an empty user ID", s.DeleteSessionsByUser(ctx, ""), storage.ErrInvalidArgument},
		)
	})
}

// writeUserSessions creates three sessions of user-a, two of them at one time,
// and sessions of users whose IDs differ from user-a's by a suffix or in case.
func writeUserSessions(t *testing.T, s storage.Storage) {
	t.Helper()
	for _, r := range []struct {
		id, user string
		created  time.Time
	}{
		{"sess-c", "user-a", minute(1)},
		{"sess-b", "user-a", minute(2)},
		{"sess-a", "user-a", minute(1)},
		{"sess-d", "user-ab", minute(3)},
		{"sess-e", "USER-A", minute(3)},
	} {
		must(t, s.CreateSession(t.Context(), fullSession(t, r.id, r.user, "cred-a", 0, r.created)))
	}
}

// fullSession is session id of userID and credentialID with every other field
// set from v: with v 0, none to its zero or default value, so it is revoked.
func fullSession(t *testing.T, id, userID, credentialID string, v int, created time.Time) *model.Session {
	t.Helper()
	s, err := model.NewSession(id, userID, credentialID,
		[]enum.Auth{enum.APIKey, enum.Password}[v%2],
		[]byte(fmt.Sprintf("refresh-%s-%d", id, v)),
		created.Add(time.Duration(24+v)*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	s.SetRevokedAt(created.Add(time.Duration(v+2) * time.Hour))
	s.SetCreatedAt(created)
	s.SetUpdatedAt(created.Add(time.Duration(v+1) * time.Minute))
	return s
}

// minimalSession is session id of userID and credentialID as its constructor
// builds it from the least it takes, no auth kind, a refresh hash of one byte
// and an expiry an hour after created, created at created: not revoked, and no
// modification time.
func minimalSession(t *testing.T, id, userID, credentialID string, created time.Time) *model.Session {
	t.Helper()
	s, err := model.NewSession(id, userID, credentialID, "", []byte{0}, created.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s.SetCreatedAt(created)
	s.SetUpdatedAt(time.Time{})
	return s
}

// liveSession is fullSession, not revoked.
func liveSession(t *testing.T, id, userID, credentialID string, v int, created time.Time) *model.Session {
	t.Helper()
	s := fullSession(t, id, userID, credentialID, v, created)
	s.SetRevokedAt(time.Time{})
	return s
}

// equalSession compares every accessor of a session read back with the session
// written.
func equalSession(t *testing.T, what string, got, want *model.Session) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil session, want %s", what, want.ID())
		return
	}
	f := fields{t, what}
	f.same("ID", got.ID(), want.ID())
	f.same("UserID", got.UserID(), want.UserID())
	f.same("CredentialID", got.CredentialID(), want.CredentialID())
	f.same("AuthKind", got.AuthKind(), want.AuthKind())
	f.same("RefreshHash", got.RefreshHash(), want.RefreshHash())
	f.same("Revoked", got.Revoked(), want.Revoked())
	f.instant("ExpiresAt", got.ExpiresAt(), want.ExpiresAt())
	f.instant("RevokedAt", got.RevokedAt(), want.RevokedAt())
	f.instant("CreatedAt", got.CreatedAt(), want.CreatedAt())
	f.instant("UpdatedAt", got.UpdatedAt(), want.UpdatedAt())
}
