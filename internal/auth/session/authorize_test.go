package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

func TestAuthorizeAccessUsesDurableState(t *testing.T) {
	ctx := context.Background()
	st := inmemory.New()
	now := time.Now()
	clk := &fakeClock{t: now}
	u := mustUser(t, "u", "subject", false)
	mustUpsertUser(t, ctx, st, u)
	sess, err := model.NewSession("s", "u", "c", enum.Password, make([]byte, 32), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	rbac := &fakeRBAC{perms: []enum.Permission{enum.SpecsGet}}
	svc := newService(t, st, clk, &fakeIssuer{}, rbac, false)
	token := &identity.Identity{UserID: "u", SessionID: "s", Permissions: []enum.Permission{enum.UsersDelete}}
	current, err := svc.AuthorizeAccess(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if current.HasPermission(enum.UsersDelete) || !current.HasPermission(enum.SpecsGet) {
		t.Fatalf("stale permissions survived: %+v", current)
	}
	if !token.HasPermission(enum.UsersDelete) {
		t.Fatal("input JWT identity mutated")
	}
	rbac.perms = nil
	if _, err = svc.AuthorizeAccess(ctx, token); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("removed permissions: %v", err)
	}
	rbac.perms = []enum.Permission{enum.SpecsGet}
	u.Disable()
	mustUpsertUser(t, ctx, st, u)
	if _, err = svc.AuthorizeAccess(ctx, token); !errors.Is(err, auth.ErrRevoked) {
		t.Fatalf("disabled account: %v", err)
	}
	u.Enable()
	mustUpsertUser(t, ctx, st, u)
	if err = st.RevokeSession(ctx, "s", now); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.AuthorizeAccess(ctx, token); !errors.Is(err, auth.ErrRevoked) {
		t.Fatalf("revoked session: %v", err)
	}
}

func TestAuthorizeAccessRejectsMissingOrForeignSession(t *testing.T) {
	svc := newService(t, inmemory.New(), &fakeClock{t: time.Now()}, &fakeIssuer{}, &fakeRBAC{}, false)
	for _, id := range []*identity.Identity{nil, {}, {UserID: "u", SessionID: "missing"}} {
		if _, err := svc.AuthorizeAccess(context.Background(), id); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("identity %+v: %v", id, err)
		}
	}
}
