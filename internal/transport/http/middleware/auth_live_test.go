package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

type fixtureVerifier struct{ id *identity.Identity }

func (v fixtureVerifier) Verify(context.Context, string) (*identity.Identity, error) {
	return v.id, nil
}

type fixtureIssuer struct{}

func (fixtureIssuer) Issue(context.Context, *identity.Identity) (string, error) { return "issued", nil }

func TestAuthChecksRevocationAndCurrentPermissionsBeforeHandler(t *testing.T) {
	ctx := context.Background()
	st := inmemory.New()
	u, err := model.NewUser("u", "user")
	if err != nil {
		t.Fatal(err)
	}
	u.Enable()
	u.SetPermissions([]string{string(enum.AgentsGet)})
	if err = st.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	sess, err := model.NewSession("s", "u", "c", enum.Password, make([]byte, 32), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	svc := session.New(st, fixtureIssuer{}, token.RealClock(), session.Config{}, rbac.NewResolver(st), nil)
	calls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		id, _ := transportctx.Identity(r.Context())
		if !id.HasPermission(enum.AgentsGet) || id.HasPermission(enum.UsersDelete) {
			t.Fatal("handler received stale JWT permissions")
		}
		w.WriteHeader(204)
	})
	handler := Auth(fixtureVerifier{&identity.Identity{UserID: "u", SessionID: "s", Permissions: []enum.Permission{enum.UsersDelete}}}, svc, notSecure, zerolog.Nop())(next)
	request := func() int {
		r := httptest.NewRequest(http.MethodGet, "/api/test", nil)
		r.Header.Set("Authorization", "Bearer valid")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if got := request(); got != 204 {
		t.Fatal(got)
	}
	if err = st.RevokeSession(ctx, "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := request(); got != 401 {
		t.Fatalf("revoked session reached handler: %d", got)
	}
	if calls != 1 {
		t.Fatalf("unauthorized handler calls: %d", calls)
	}
}
