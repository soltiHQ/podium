package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// validKind is the workload of a spec that passes validation.
const validKind = `"kind_type":"subprocess","kind_config":{"mode":{"command":{"command":"echo"}}}`

// Endpoints that read a JSON body require Content-Type application/json, a
// type an HTML form cannot send: anything else is 415 and changes nothing.
func TestDecodeJSONRequiresContentType(t *testing.T) {
	admin := &identity.Identity{UserID: "admin", Permissions: enum.All}
	for _, e := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/v1/users", `{"subject":"new-user"}`},
		{http.MethodPut, "/api/v1/users/target", `{"name":"Renamed"}`},
		{http.MethodPost, "/api/v1/users/target/password", `{"password":"content-type-password"}`},
		{http.MethodPut, "/api/v1/agents/agent-1/labels", `{"env":"prod"}`},
		{http.MethodPost, "/api/v1/specs", `{"name":"New spec","slot":"new-slot",` + validKind + `}`},
		{http.MethodPut, "/api/v1/specs/spec-1", `{"version":1,"name":"Renamed",` + validKind + `}`},
	} {
		t.Run(e.method+" "+e.target, func(t *testing.T) {
			f := newAPIFixture(t)
			f.user(t, "target")
			f.spec(t, "spec-1")
			for _, contentType := range []string{
				"",
				"text/plain",
				"application/x-www-form-urlencoded",
				"multipart/form-data; boundary=x",
				"application/json-patch+json",
				"application/json; charset=",
			} {
				w := f.send(e.method, e.target, contentType, e.body, admin)
				if w.Code != http.StatusUnsupportedMediaType {
					t.Fatalf("Content-Type %q: HTTP %d %s, want 415", contentType, w.Code, w.Body.String())
				}
				var got struct {
					Message string `json:"message"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Message != "Content-Type must be application/json." {
					t.Fatalf("Content-Type %q: message %q (%v)", contentType, got.Message, err)
				}
			}
			if _, err := f.store.GetUserBySubject(context.Background(), "new-user"); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("a refused body created a user: %v", err)
			}
			// The suite sends bare application/json; parameters are allowed too.
			if w := f.send(e.method, e.target, "application/json; charset=utf-8", e.body, admin); w.Code == http.StatusUnsupportedMediaType || w.Code == http.StatusBadRequest {
				t.Fatalf("Content-Type with a charset: HTTP %d %s, want the body read", w.Code, w.Body.String())
			}
		})
	}
}
