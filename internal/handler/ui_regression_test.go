package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/service/spec"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

func TestSafeLoginRedirect(t *testing.T) {
	for _, raw := range []string{"https://evil.test", "//evil.test", "/\\evil.test", "/%2f%2fevil.test", "/%5cevil.test", "/\r\nLocation:evil"} {
		if got := safeLoginRedirect(raw); got != "/" {
			t.Fatalf("unsafe redirect %q accepted as %q", raw, got)
		}
	}
	if got := safeLoginRedirect("/specs?tab=all"); got != "/specs?tab=all" {
		t.Fatal(got)
	}
}

func TestSpecEditMissingDoesNotBecomeCreateForm(t *testing.T) {
	u := &UI{specSVC: spec.New(inmemory.New(), zerolog.Nop())}
	r := httptest.NewRequest(http.MethodGet, "/specs/edit/missing", nil)
	r = r.WithContext(transportctx.WithIdentity(r.Context(), &identity.Identity{UserID: "u"}))
	w := httptest.NewRecorder()
	u.SpecEdit(w, r)
	if w.Code != 404 {
		t.Fatalf("missing edit status %d: %s", w.Code, w.Body.String())
	}
}

func TestDecodeJSONRejectsTrailingValues(t *testing.T) {
	for _, input := range []string{`{"name":"a"} {"name":"b"}`, `{"name":"a"} garbage`} {
		if _, err := decodeJSON[map[string]string](httptest.NewRequest(http.MethodPost, "/", strings.NewReader(input))); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestSelfAccountGuardsRunBeforeMutation(t *testing.T) {
	a := &API{}
	for _, action := range []string{"delete", "disable"} {
		r := httptest.NewRequest(http.MethodDelete, "/", nil).WithContext(transportctx.WithIdentity(context.Background(), &identity.Identity{UserID: "me"}))
		w := httptest.NewRecorder()
		if action == "delete" {
			a.userDelete(w, r, httpctx.RenderBlock, "me")
		} else {
			a.userSetStatus(w, r, httpctx.RenderBlock, "me", userDisable)
		}
		if w.Code != 400 {
			t.Fatalf("%s status %d", action, w.Code)
		}
	}
}
