package inmemory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestStore_Agents_CRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()
	if err := s.UpsertAgent(ctx, nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	a := mkAgent(t, "a1")
	requireNoErr(t, s.UpsertAgent(ctx, a))

	got, err := s.GetAgent(ctx, a.ID())
	requireNoErr(t, err)
	requireNotNil(t, got)
	if got.ID() != a.ID() {
		t.Fatalf("unexpected id: %q != %q", got.ID(), a.ID())
	}

	_, err = s.GetAgent(ctx, "missing")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, err=%v", err)
	}

	requireNoErr(t, s.DeleteAgent(ctx, a.ID()))

	if err = s.DeleteAgent(ctx, a.ID()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, err=%v", err)
	}
}

func TestStore_Agents_List_FilterTypeValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()

	requireNoErr(t, s.UpsertAgent(ctx, mkAgent(t, "a1")))

	type foreignFilter struct{}
	var _ storage.AgentFilter = (*foreignFilter)(nil)

	_, err := s.ListAgents(ctx, &foreignFilter{}, storage.ListOptions{})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
}

func TestStore_Users_CRUD_AndGetBySubject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()
	if err := s.UpsertUser(ctx, nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	u := mkUser(t, "u1", "sub-1")
	requireNoErr(t, s.UpsertUser(ctx, u))

	got, err := s.GetUser(ctx, u.ID())
	requireNoErr(t, err)
	requireNotNil(t, got)
	if got.ID() != u.ID() || got.Subject() != u.Subject() {
		t.Fatalf("unexpected user")
	}

	_, err = s.GetUserBySubject(ctx, "")
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	got2, err := s.GetUserBySubject(ctx, u.Subject())
	requireNoErr(t, err)
	requireNotNil(t, got2)
	if got2.ID() != u.ID() {
		t.Fatalf("unexpected id: %q != %q", got2.ID(), u.ID())
	}

	_, err = s.GetUserBySubject(ctx, "missing-sub")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, err=%v", err)
	}
}

func TestStore_Users_GetBySubject_NonUnique_ReturnsInternal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()

	u1 := mkUser(t, "u1", "dup-sub")
	u2 := mkUser(t, "u2", "dup-sub")
	requireNoErr(t, s.UpsertUser(ctx, u1))
	requireNoErr(t, s.UpsertUser(ctx, u2))

	_, err := s.GetUserBySubject(ctx, "dup-sub")
	if !errors.Is(err, storage.ErrInternal) {
		t.Fatalf("expected ErrInternal, err=%v", err)
	}
}

func TestStore_Credentials_CRUD_AndByUserAuth(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()

	if err := s.UpsertCredential(ctx, nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	u := mkUser(t, "u1", "sub-1")
	requireNoErr(t, s.UpsertUser(ctx, u))

	c1 := mkCredential(t, "c1", u.ID(), kind.Password)
	requireNoErr(t, s.UpsertCredential(ctx, c1))

	got, err := s.GetCredential(ctx, c1.ID())
	requireNoErr(t, err)
	requireNotNil(t, got)
	if got.ID() != c1.ID() {
		t.Fatalf("unexpected id")
	}

	_, err = s.GetCredentialByUserAndAuth(ctx, "", kind.Password)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	got2, err := s.GetCredentialByUserAndAuth(ctx, u.ID(), kind.Password)
	requireNoErr(t, err)
	requireNotNil(t, got2)
	if got2.ID() != c1.ID() {
		t.Fatalf("unexpected credential id")
	}

	_, err = s.GetCredentialByUserAndAuth(ctx, u.ID(), kind.APIKey)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, err=%v", err)
	}
}

func TestStore_Credentials_ByUserAuth_NonUnique_ReturnsInternal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()

	u := mkUser(t, "u1", "sub-1")
	requireNoErr(t, s.UpsertUser(ctx, u))

	c1 := mkCredential(t, "c1", u.ID(), kind.Password)
	c2 := mkCredential(t, "c2", u.ID(), kind.Password)
	requireNoErr(t, s.UpsertCredential(ctx, c1))
	requireNoErr(t, s.UpsertCredential(ctx, c2))

	_, err := s.GetCredentialByUserAndAuth(ctx, u.ID(), kind.Password)
	if !errors.Is(err, storage.ErrInternal) {
		t.Fatalf("expected ErrInternal, err=%v", err)
	}
}

func TestStore_Credentials_ListByUser(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()

	_, err := s.ListCredentialsByUser(ctx, "")
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	u1 := mkUser(t, "u1", "sub-1")
	u2 := mkUser(t, "u2", "sub-2")
	requireNoErr(t, s.UpsertUser(ctx, u1))
	requireNoErr(t, s.UpsertUser(ctx, u2))

	requireNoErr(t, s.UpsertCredential(ctx, mkCredential(t, "c1", u1.ID(), kind.Password)))
	requireNoErr(t, s.UpsertCredential(ctx, mkCredential(t, "c2", u1.ID(), kind.APIKey)))
	requireNoErr(t, s.UpsertCredential(ctx, mkCredential(t, "c3", u2.ID(), kind.Password)))

	list, err := s.ListCredentialsByUser(ctx, u1.ID())
	requireNoErr(t, err)
	if len(list) != 2 {
		t.Fatalf("expected 2, got %d", len(list))
	}
	for _, c := range list {
		if c.UserID() != u1.ID() {
			t.Fatalf("unexpected user id in credential list")
		}
	}
}

func TestStore_Verifiers_CRUD_AndByCredential(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()

	if err := s.UpsertVerifier(ctx, nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	u := mkUser(t, "u1", "sub-1")
	requireNoErr(t, s.UpsertUser(ctx, u))
	c := mkCredential(t, "c1", u.ID(), kind.Password)
	requireNoErr(t, s.UpsertCredential(ctx, c))

	v := mkVerifier(t, "v1", c.ID(), kind.Password)
	requireNoErr(t, s.UpsertVerifier(ctx, v))

	got, err := s.GetVerifier(ctx, v.ID())
	requireNoErr(t, err)
	requireNotNil(t, got)

	_, err = s.GetVerifierByCredential(ctx, "")
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	got2, err := s.GetVerifierByCredential(ctx, c.ID())
	requireNoErr(t, err)
	requireNotNil(t, got2)
	if got2.ID() != v.ID() {
		t.Fatalf("unexpected verifier id")
	}
}

func TestStore_Verifiers_ByCredential_NonUnique_ReturnsInternal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()

	u := mkUser(t, "u1", "sub-1")
	requireNoErr(t, s.UpsertUser(ctx, u))
	c := mkCredential(t, "c1", u.ID(), kind.Password)
	requireNoErr(t, s.UpsertCredential(ctx, c))

	v1 := mkVerifier(t, "v1", c.ID(), kind.Password)
	v2 := mkVerifier(t, "v2", c.ID(), kind.Password)
	requireNoErr(t, s.UpsertVerifier(ctx, v1))
	requireNoErr(t, s.UpsertVerifier(ctx, v2))

	_, err := s.GetVerifierByCredential(ctx, c.ID())
	if !errors.Is(err, storage.ErrInternal) {
		t.Fatalf("expected ErrInternal, err=%v", err)
	}
}

func TestStore_Sessions_CRUD_Rotate_Revoke(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()

	if err := s.CreateSession(ctx, nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	u := mkUser(t, "u1", "sub-1")
	requireNoErr(t, s.UpsertUser(ctx, u))
	c := mkCredential(t, "c1", u.ID(), kind.Password)
	requireNoErr(t, s.UpsertCredential(ctx, c))

	sess := mkSession(t, "s1", u.ID(), c.ID(), kind.Password)
	requireNoErr(t, s.CreateSession(ctx, sess))

	if err := s.CreateSession(ctx, sess); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, err=%v", err)
	}

	got, err := s.GetSession(ctx, sess.ID())
	requireNoErr(t, err)
	requireNotNil(t, got)
	if got.ID() != sess.ID() {
		t.Fatalf("unexpected session id")
	}

	if err = s.RotateRefresh(ctx, "", []byte("x"), fixedNow().Add(time.Hour)); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
	if err = s.RotateRefresh(ctx, sess.ID(), nil, fixedNow().Add(time.Hour)); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
	if err = s.RotateRefresh(ctx, sess.ID(), []byte("x"), time.Time{}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	newHash := []byte("new-hash")
	newExp := fixedNow().Add(48 * time.Hour)
	requireNoErr(t, s.RotateRefresh(ctx, sess.ID(), newHash, newExp))

	got2, err := s.GetSession(ctx, sess.ID())
	requireNoErr(t, err)
	if got2.ExpiresAt() != newExp {
		t.Fatalf("expiresAt not updated")
	}

	if err = s.RevokeSession(ctx, "", fixedNow()); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
	if err = s.RevokeSession(ctx, sess.ID(), time.Time{}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	revAt := fixedNow().Add(time.Minute)
	requireNoErr(t, s.RevokeSession(ctx, sess.ID(), revAt))

	got3, err := s.GetSession(ctx, sess.ID())
	requireNoErr(t, err)
	if got3.RevokedAt().IsZero() {
		t.Fatalf("expected revokedAt set")
	}

	requireNoErr(t, s.DeleteSession(ctx, sess.ID()))
	if _, err = s.GetSession(ctx, sess.ID()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, err=%v", err)
	}
}

func TestStore_Roles_CRUD_GetByName_GetRolesOrdering(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()

	if err := s.UpsertRole(ctx, nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	r1 := mkRole(t, "r1", "admin")
	r2 := mkRole(t, "r2", "viewer")

	requireNoErr(t, s.UpsertRole(ctx, r1))
	requireNoErr(t, s.UpsertRole(ctx, r2))

	_, err := s.GetRoleByName(ctx, "")
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	got, err := s.GetRoleByName(ctx, "admin")
	requireNoErr(t, err)
	if got.ID() != r1.ID() {
		t.Fatalf("unexpected role id")
	}

	r3 := mkRole(t, "r3", "admin")
	requireNoErr(t, s.UpsertRole(ctx, r3))
	_, err = s.GetRoleByName(ctx, "admin")
	if !errors.Is(err, storage.ErrInternal) {
		t.Fatalf("expected ErrInternal, err=%v", err)
	}

	_, err = s.GetRoles(ctx, nil)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
	_, err = s.GetRoles(ctx, []string{"", "x"})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	out, err := s.GetRoles(ctx, []string{"r2", "r1", "r2"})
	requireNoErr(t, err)
	if len(out) != 3 {
		t.Fatalf("expected 3 roles, got %d", len(out))
	}
	if out[0].ID() != "r2" || out[1].ID() != "r1" || out[2].ID() != "r2" {
		t.Fatalf("order/dup semantics broken: got %q,%q,%q", out[0].ID(), out[1].ID(), out[2].ID())
	}
}

func TestStore_Roles_GetRoles_MissingID_ReturnsNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()

	requireNoErr(t, s.UpsertRole(ctx, mkRole(t, "r1", "admin")))

	_, err := s.GetRoles(ctx, []string{"r1", "missing"})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, err=%v", err)
	}
}
