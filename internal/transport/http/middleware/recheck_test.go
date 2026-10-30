package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// recheckRequest carries the identity Auth stores for user "u" on session
// "s-secret". The identity still lists AgentsGet: Recheck must go by the
// stored user, not by what the request started with.
func recheckRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil)
	id := &identity.Identity{UserID: "u", SessionID: "s-secret", Permissions: []enum.Permission{enum.AgentsGet}}
	return r.WithContext(transportctx.WithIdentity(r.Context(), id))
}

// recheckStore holds user "u", enabled with perms, and its live session.
func recheckStore(t *testing.T, perms ...enum.Permission) (*inmemory.Store, *model.User) {
	t.Helper()
	ctx := context.Background()
	st := inmemory.New()
	u, err := model.NewUser("u", "user")
	if err != nil {
		t.Fatal(err)
	}
	u.Enable()
	names := make([]string, 0, len(perms))
	for _, p := range perms {
		names = append(names, string(p))
	}
	u.SetPermissions(names)
	if err = st.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	sess, err := model.NewSession("s-secret", "u", "c", enum.Password, make([]byte, 32), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return st, u
}

func recheckService(st storage.Storage) *session.Service {
	return session.New(st, fixtureIssuer{}, token.RealClock(), session.Config{}, rbac.NewResolver(st), nil)
}

// Recheck repeats the access check of Auth, and of RequirePermission when it
// names a permission, against the stored session and user.
func TestRecheckFollowsStoredAccess(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		perm   enum.Permission
		change func(t *testing.T, st *inmemory.Store, u *model.User)
		want   error
	}{
		{name: "access unchanged", perm: enum.AgentsGet},
		{name: "access unchanged, authentication only"},
		{name: "session revoked", perm: enum.AgentsGet, want: auth.ErrRevoked, change: func(t *testing.T, st *inmemory.Store, _ *model.User) {
			if err := st.RevokeSession(ctx, "s-secret", time.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "session revoked, authentication only", want: auth.ErrRevoked, change: func(t *testing.T, st *inmemory.Store, _ *model.User) {
			if err := st.RevokeSession(ctx, "s-secret", time.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "user disabled", perm: enum.AgentsGet, want: auth.ErrRevoked, change: func(t *testing.T, st *inmemory.Store, u *model.User) {
			u.Disable()
			if err := st.UpsertUser(ctx, u); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "user deleted", want: auth.ErrInvalidToken, change: func(t *testing.T, st *inmemory.Store, u *model.User) {
			if err := st.DeleteUser(ctx, u.ID()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "permission removed", perm: enum.AgentsGet, want: auth.ErrUnauthorized, change: func(t *testing.T, st *inmemory.Store, u *model.User) {
			u.SetPermissions([]string{string(enum.SpecsGet)})
			if err := st.UpsertUser(ctx, u); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "permission removed, authentication only", change: func(t *testing.T, st *inmemory.Store, u *model.User) {
			u.SetPermissions([]string{string(enum.SpecsGet)})
			if err := st.UpsertUser(ctx, u); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, u := recheckStore(t, enum.AgentsGet, enum.SpecsGet)
			check := Recheck(recheckService(st), tc.perm, zerolog.Nop())
			if err := check(recheckRequest()); err != nil {
				t.Fatalf("before the change: %v, want access", err)
			}
			if tc.change != nil {
				tc.change(t, st, u)
			}
			err := check(recheckRequest())
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("after the change: %v, want access", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("after the change: %v, want %v", err, tc.want)
			}
		})
	}
}

// A request without an identity fails the recheck: nothing let it in.
func TestRecheckWithoutIdentityFails(t *testing.T) {
	st, _ := recheckStore(t, enum.AgentsGet)
	check := Recheck(recheckService(st), "", zerolog.Nop())
	if err := check(httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil)); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("recheck without identity: %v, want %v", err, auth.ErrUnauthorized)
	}
}

// Credentials Recheck cannot check fail it (fail closed) and are logged like
// in Auth: one ERROR line with the method, the path and the error, never a
// credential.
func TestRecheckFailsClosedOnStorageErrors(t *testing.T) {
	st, _ := recheckStore(t, enum.AgentsGet)
	var logs bytes.Buffer
	check := Recheck(recheckService(sessionOutage{st}), enum.AgentsGet, zerolog.New(&logs))
	if err := check(recheckRequest()); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("recheck during a storage outage: %v, want %v", err, storage.ErrUnavailable)
	}
	var entry struct {
		Level  string `json:"level"`
		Method string `json:"method"`
		Path   string `json:"path"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry.Level != "error" || entry.Method != http.MethodGet ||
		entry.Path != "/api/v1/events/stream" || entry.Error != storage.ErrUnavailable.Error() {
		t.Fatalf("log %q, want one ERROR line with the method, the path and the error", logs.String())
	}
	if strings.Contains(logs.String(), "s-secret") {
		t.Fatalf("log holds the session ID: %s", logs.String())
	}
}
