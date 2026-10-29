package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

func TestUserCreationSeparatelyAuthorizesAccessAssignments(t *testing.T) {
	st := inmemory.New()
	a := &API{userSVC: user.New(st, zerolog.Nop()), hub: event.NewHub(zerolog.Nop())}
	request := func(body string, permissions ...enum.Permission) int {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(body))
		r = r.WithContext(transportctx.WithIdentity(r.Context(), &identity.Identity{UserID: "actor", Permissions: permissions}))
		w := httptest.NewRecorder()
		a.Users(w, r)
		return w.Code
	}
	for _, body := range []string{`{"subject":"forbidden","role_ids":["admin"]}`, `{"subject":"forbidden","permissions":["users:delete"]}`} {
		if got := request(body, enum.UsersAdd); got != 403 {
			t.Fatalf("forbidden assignment status %d", got)
		}
		if _, err := st.GetUserBySubject(context.Background(), "forbidden"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("forbidden request mutated users: %v", err)
		}
	}
	if got := request(`{"subject":"ordinary"}`, enum.UsersAdd); got != 204 {
		t.Fatalf("ordinary create status %d", got)
	}
	if got := request(`{"subject":"assigned","permissions":["agents:get"]}`, enum.UsersAdd, enum.UsersEdit); got != 204 {
		t.Fatalf("admin create status %d", got)
	}
	created, err := st.GetUserBySubject(context.Background(), "assigned")
	if err != nil {
		t.Fatal(err)
	}
	if got := created.PermissionsAll(); len(got) != 1 || got[0] != enum.AgentsGet {
		t.Fatalf("admin assignment lost: %+v", got)
	}
}
