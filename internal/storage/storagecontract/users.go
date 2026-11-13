package storagecontract

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// users checks the user rows: the round trip of a full and of a minimal row,
// replacement, the rows a store refuses, deletion, the lookup by subject and
// the filter the factory builds.
func users(t *testing.T, newStore storeFunc) {
	t.Run("round trip", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := fullUser(t, "user-a", "sub-a", 0, minute(0))
		must(t, s.UpsertUser(ctx, want))

		got, err := s.GetUser(ctx, want.ID())
		gotRow(t, "GetUser", got, err, want, equalUser)
		got, err = s.GetUserBySubject(ctx, want.Subject())
		gotRow(t, "GetUserBySubject", got, err, want, equalUser)
		listed, err := listedUsers(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListUsers", listed, []*model.User{want}, equalUser)
	})

	t.Run("round trip of a minimal row", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := minimalUser(t, "user-a", "sub-a", minute(0))
		must(t, s.UpsertUser(ctx, want))

		got, err := s.GetUser(ctx, want.ID())
		gotRow(t, "GetUser", got, err, want, equalUser)
		got, err = s.GetUserBySubject(ctx, want.Subject())
		gotRow(t, "GetUserBySubject", got, err, want, equalUser)
		listed, err := listedUsers(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListUsers", listed, []*model.User{want}, equalUser)
	})

	t.Run("upsert replaces", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		first, other := fullUser(t, "user-a", "sub-a", 0, minute(0)), fullUser(t, "user-b", "sub-b", 0, minute(3))
		must(t, errors.Join(s.UpsertUser(ctx, first), s.UpsertUser(ctx, other)))

		// Every field changes, the subject and the creation time too: the row lists
		// at its new time, and the lookup finds it by its new subject alone. That
		// CreatedAt never changes is a rule for callers, not the store, which keeps
		// no earlier one.
		want := fullUser(t, "user-a", "sub-z", 1, minute(5))
		must(t, s.UpsertUser(ctx, want))
		got, err := s.GetUser(ctx, want.ID())
		gotRow(t, "GetUser", got, err, want, equalUser)
		got, err = s.GetUserBySubject(ctx, "sub-z")
		gotRow(t, "GetUserBySubject of the new subject", got, err, want, equalUser)
		expect(t, outcome{"GetUserBySubject of the old subject", errOf(s.GetUserBySubject(ctx, "sub-a")), storage.ErrNotFound})
		listed, err := listedUsers(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListUsers", listed, []*model.User{want, other}, equalUser)
	})

	t.Run("invalid rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		noID := &model.User{}
		noID.SetCreatedAt(minute(0))
		noCreation := fullUser(t, "user-a", "sub-a", 0, minute(0))
		noCreation.SetCreatedAt(time.Time{})
		expect(t,
			outcome{"UpsertUser of nil", s.UpsertUser(ctx, nil), storage.ErrInvalidArgument},
			outcome{"UpsertUser of a row without an ID", s.UpsertUser(ctx, noID), storage.ErrInvalidArgument},
			outcome{"UpsertUser of a row without a creation time", s.UpsertUser(ctx, noCreation), storage.ErrInvalidArgument},
		)
		listed, err := listedUsers(ctx, s, nil)
		sameIDs(t, "ListUsers after the refused writes", listed, err)
	})

	t.Run("delete and missing rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		kept := fullUser(t, "user-b", "sub-b", 0, minute(0))
		must(t, errors.Join(s.UpsertUser(ctx, fullUser(t, "user-a", "sub-a", 0, minute(0))), s.UpsertUser(ctx, kept)))
		expect(t,
			outcome{"DeleteUser", s.DeleteUser(ctx, "user-a"), nil},
			outcome{"GetUser of a deleted row", errOf(s.GetUser(ctx, "user-a")), storage.ErrNotFound},
			outcome{"GetUserBySubject of a deleted row", errOf(s.GetUserBySubject(ctx, "sub-a")), storage.ErrNotFound},
			outcome{"DeleteUser of a deleted row", s.DeleteUser(ctx, "user-a"), storage.ErrNotFound},
			outcome{"GetUser of an ID never written", errOf(s.GetUser(ctx, "user-x")), storage.ErrNotFound},
			outcome{"DeleteUser of an ID never written", s.DeleteUser(ctx, "user-x"), storage.ErrNotFound},
			outcome{"GetUser of an empty ID", errOf(s.GetUser(ctx, "")), storage.ErrInvalidArgument},
			outcome{"DeleteUser of an empty ID", s.DeleteUser(ctx, ""), storage.ErrInvalidArgument},
		)
		got, err := s.GetUser(ctx, kept.ID())
		gotRow(t, "GetUser of the row kept", got, err, kept, equalUser)
	})

	t.Run("lookup by subject", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// Subjects that differ from sub-a by a suffix or in case: an exact
		// comparison finds sub-a's row alone.
		want := fullUser(t, "user-a", "sub-a", 0, minute(0))
		must(t, errors.Join(
			s.UpsertUser(ctx, want),
			s.UpsertUser(ctx, fullUser(t, "user-b", "sub-ab", 0, minute(0))),
			s.UpsertUser(ctx, fullUser(t, "user-c", "SUB-A", 0, minute(0))),
		))
		got, err := s.GetUserBySubject(ctx, "sub-a")
		gotRow(t, "GetUserBySubject", got, err, want, equalUser)
		expect(t,
			outcome{"GetUserBySubject of a prefix", errOf(s.GetUserBySubject(ctx, "sub-")), storage.ErrNotFound},
			outcome{"GetUserBySubject with spaces around it", errOf(s.GetUserBySubject(ctx, " sub-a ")), storage.ErrNotFound},
			outcome{"GetUserBySubject of a subject never written", errOf(s.GetUserBySubject(ctx, "sub-x")), storage.ErrNotFound},
			outcome{"GetUserBySubject of an empty subject", errOf(s.GetUserBySubject(ctx, "")), storage.ErrInvalidArgument},
		)
		// The subject is indexed, not unique: two rows of one subject are a fault.
		must(t, s.UpsertUser(ctx, fullUser(t, "user-d", "sub-a", 1, minute(1))))
		expect(t, outcome{"GetUserBySubject of a subject two rows hold", errOf(s.GetUserBySubject(ctx, "sub-a")), storage.ErrInternal})
	})

	t.Run("filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		for _, r := range []struct{ id, subject, name, email string }{
			{"user-kilo", "sub-lima", "Mike Zone1", "november@example.net"},
			{"user-oscar", "sub-papa_quebec", "Romeo Zone1", "sierra@example.net"},
			{"user-tango", "sub-uniform", "Victor 100%", "whiskey@example.net"},
			{"user-xray", "sub-papaxquebec", "Yankee", "zulu@example.net"},
			{"user-alfa", "sub-bravo", "Ärzte", "charlie@example.net"},
			{"user-delta", "sub-echo", `share a\b`, "foxtrot@example.net"},
			// Its name holds a\b without the backslash.
			{"user-golf", "sub-hotel", "slab", "india@example.net"},
		} {
			u := fullUser(t, r.id, r.subject, 0, minute(0))
			u.SetName(r.name)
			u.SetEmail(r.email)
			u.SetUpdatedAt(minute(1))
			must(t, s.UpsertUser(ctx, u))
		}
		every := []string{"user-alfa", "user-delta", "user-golf", "user-kilo", "user-oscar", "user-tango", "user-xray"}
		listed, err := listedUsers(ctx, s, nil)
		sameIDs(t, "ListUsers with no filter", listed, err, every...)
		for _, tc := range []struct {
			name string
			c    storage.UserQueryCriteria
			want []string
		}{
			{"empty criteria", storage.UserQueryCriteria{}, every},
			{"query on the subject, in another case", storage.UserQueryCriteria{Query: "LIMA"}, []string{"user-kilo"}},
			{"query on the name, in another case", storage.UserQueryCriteria{Query: "mIKE"}, []string{"user-kilo"}},
			{"query on the email, in another case", storage.UserQueryCriteria{Query: "NOVEMBER"}, []string{"user-kilo"}},
			{"query on the ID, not searched", storage.UserQueryCriteria{Query: "kilo"}, nil},
			{"query matching two rows", storage.UserQueryCriteria{Query: "zone1"}, []string{"user-kilo", "user-oscar"}},
			// The spec is silent on spaces around a query: the in-memory store trims
			// them, so a query of spaces alone is no query.
			{"query with spaces around it", storage.UserQueryCriteria{Query: "  lima  "}, []string{"user-kilo"}},
			{"query of spaces", storage.UserQueryCriteria{Query: "   "}, every},
			{"query with an underscore, matched as one", storage.UserQueryCriteria{Query: "papa_q"}, []string{"user-oscar"}},
			{"query with a percent sign, matched as one", storage.UserQueryCriteria{Query: "%"}, []string{"user-tango"}},
			// The spec is silent on case beyond ASCII and on a backslash: the
			// in-memory store folds case as strings.ToLower does and matches a
			// backslash as itself, a query that ends in one included.
			{"query in another case, beyond ASCII", storage.UserQueryCriteria{Query: "äRZTE"}, []string{"user-alfa"}},
			{"query with a backslash, matched as one", storage.UserQueryCriteria{Query: `a\b`}, []string{"user-delta"}},
			{"query that ends in a backslash, matched as one", storage.UserQueryCriteria{Query: `a\`}, []string{"user-delta"}},
		} {
			listed, err := listedUsers(ctx, s, s.BuildUserFilter(tc.c))
			sameIDs(t, "ListUsers, "+tc.name, listed, err, tc.want...)
		}
	})

	t.Run("foreign filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		must(t, s.UpsertUser(ctx, fullUser(t, "user-a", "sub-a", 0, minute(0))))
		expect(t,
			outcome{"ListUsers with a filter of no backend", errOf(s.ListUsers(ctx, foreignFilter{}, storage.ListOptions{})), storage.ErrInvalidArgument},
			outcome{"ListUsers with an agent filter", errOf(s.ListUsers(ctx, s.BuildAgentFilter(storage.AgentQueryCriteria{}), storage.ListOptions{})), storage.ErrInvalidArgument},
		)
	})
}

// listedUsers drains ListUsers with filter.
func listedUsers(ctx context.Context, s storage.Storage, filter storage.UserFilter) ([]*model.User, error) {
	return storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.UserListResult, error) {
		return s.ListUsers(ctx, filter, o)
	})
}

// fullUser is user id of subject with every other field set from v: with v 0,
// none to its zero or default value.
func fullUser(t *testing.T, id, subject string, v int, created time.Time) *model.User {
	t.Helper()
	u, err := model.NewUser(id, subject)
	if err != nil {
		t.Fatal(err)
	}
	u.SetEmail(fmt.Sprintf("%s.%d@example.net", id, v))
	u.SetName(fmt.Sprintf("User %s %d", id, v))
	u.SetRoleIDs([]string{fmt.Sprintf("role-%d", v), fmt.Sprintf("role-%d", v+10)})
	u.SetPermissions([]string{string(enum.All[v]), string(enum.All[v+5])})
	if v%2 == 0 {
		u.Disable()
	}
	u.SetCreatedAt(created)
	u.SetUpdatedAt(created.Add(time.Duration(v+1) * time.Minute))
	return u
}

// minimalUser is user id of subject as its constructor builds it, created at
// created: every other field zero, and no modification time.
//
// The zero UpdatedAt is a pin the spec is silent on: see the note on the
// minimal rows in contract.go.
func minimalUser(t *testing.T, id, subject string, created time.Time) *model.User {
	t.Helper()
	u, err := model.NewUser(id, subject)
	if err != nil {
		t.Fatal(err)
	}
	u.SetCreatedAt(created)
	u.SetUpdatedAt(time.Time{})
	return u
}

// equalUser compares every accessor of a user read back with the user written.
func equalUser(t *testing.T, what string, got, want *model.User) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil user, want %s", what, want.ID())
		return
	}
	f := fields{t, what}
	f.same("ID", got.ID(), want.ID())
	f.same("Subject", got.Subject(), want.Subject())
	f.same("Email", got.Email(), want.Email())
	f.same("Name", got.Name(), want.Name())
	f.same("Disabled", got.Disabled(), want.Disabled())
	f.same("RoleIDsAll", got.RoleIDsAll(), want.RoleIDsAll())
	f.same("PermissionsAll", got.PermissionsAll(), want.PermissionsAll())
	f.instant("CreatedAt", got.CreatedAt(), want.CreatedAt())
	f.instant("UpdatedAt", got.UpdatedAt(), want.UpdatedAt())
}
