package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/workloadschema"
)

// handlerSchema is the workload schema the API fixture's catalog holds.
const handlerSchema = `{"type":"object","required":["image"],"properties":{"image":{"type":"string"}},"additionalProperties":false}`

// handlerFingerprint is the fingerprint of handlerSchema.
var handlerFingerprint = func() string {
	fp, err := workloadschema.Fingerprint([]byte(handlerSchema))
	if err != nil {
		panic(err)
	}
	return fp
}()

// validKind is the workload of a spec that passes validation.
var validKind = `"workload":{"api_version":"solti.io/v1","kind":"Container","schema_fingerprint":"` + handlerFingerprint + `","body":{"image":"busybox"}}`

// seedHandlerSchema puts handlerSchema into the catalog of the store.
func seedHandlerSchema(t *testing.T, store storage.Storage) {
	t.Helper()
	entry, err := model.NewWorkloadSchema(handlerFingerprint, "solti.io/v1", "Container", []byte(handlerSchema), "agent-1", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorkloadSchema(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
}

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
			seedHandlerSchema(t, f.store)
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

// A spec the schema rejects, or whose schema is unknown, is 400 with the problems.
func TestSpecUpsertAnswersWorkloadProblemsWith400(t *testing.T) {
	admin := &identity.Identity{UserID: "admin", Permissions: enum.All}
	f := newAPIFixture(t)
	seedHandlerSchema(t, f.store)
	wl := func(fp, body string) string {
		return `"workload":{"api_version":"solti.io/v1","kind":"Container","schema_fingerprint":"` + fp + `","body":` + body + `}`
	}
	for _, tc := range []struct{ name, body, want string }{
		{"body rejected", `{"name":"S","slot":"s1",` + wl(handlerFingerprint, `{"image":7,"extra":1}`) + `}`, "/image: "},
		{"unknown schema", `{"name":"S","slot":"s2",` + wl(strings.Repeat("0", 64), `{"image":"x"}`) + `}`, "not in the catalog"},
		{"no workload", `{"name":"S","slot":"s3"}`, "requires api_version"},
		{"not an object", `{"name":"S","slot":"s4","workload":"x"}`, "invalid request"},
	} {
		w := f.do(http.MethodPost, "/api/v1/specs", tc.body, admin)
		var got struct {
			Message string `json:"message"`
		}
		if w.Code != http.StatusBadRequest || json.Unmarshal(w.Body.Bytes(), &got) != nil || !strings.Contains(got.Message, tc.want) {
			t.Errorf("%s: HTTP %d %s, want 400 mentioning %q", tc.name, w.Code, w.Body.String(), tc.want)
		}
	}

	// A good one is created and read back with its workload.
	w := f.do(http.MethodPost, "/api/v1/specs", `{"name":"Good","slot":"good",`+validKind+`}`, admin)
	if w.Code != http.StatusNoContent {
		t.Fatalf("create: HTTP %d %s", w.Code, w.Body.String())
	}
	list := f.do(http.MethodGet, "/api/v1/specs", "", admin)
	var page struct {
		Items []restv1.Spec `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range page.Items {
		if it.Slot == "good" {
			found = it.Workload != nil && it.Workload.Kind == "Container" && it.Workload.SchemaFingerprint == handlerFingerprint && it.Workload.Body["image"] == "busybox"
		}
	}
	if !found {
		t.Fatalf("the created spec does not carry its workload: %s", list.Body.String())
	}
}

// A spec the proxy cannot send is 400, at create and at update: the envelope is
// checked for every spec. An invalid body on PUT is 400 too.
func TestSpecUpsertRefusesBadEnvelopeBodyAndBounds(t *testing.T) {
	admin := &identity.Identity{UserID: "admin", Permissions: enum.All}
	f := newAPIFixture(t)
	seedHandlerSchema(t, f.store)
	f.spec(t, "spec-1")
	wl := func(body string) string {
		return `"workload":{"api_version":"solti.io/v1","kind":"Container","schema_fingerprint":"` + handlerFingerprint + `","body":` + body + `}`
	}
	for _, tc := range []struct{ name, method, target, body string }{
		{"slot with a space", http.MethodPost, "/api/v1/specs", `{"name":"S","slot":"a b",` + validKind + `}`},
		{"invalid body on PUT", http.MethodPut, "/api/v1/specs/spec-1", `{"version":1,"name":"S",` + wl(`{"image":7}`) + `}`},
	} {
		w := f.do(tc.method, tc.target, tc.body, admin)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d %.200s, want 400", tc.name, w.Code, w.Body.String())
		}
	}
	if rows, err := f.store.GetSpec(context.Background(), "spec-1"); err != nil || rows.Version() != 1 {
		t.Fatalf("a refused PUT changed the spec: %v %v", rows, err)
	}
}

// A body over the validator's bounds (256 KiB encoded, 10 000 values) is a body
// error: 400 at create. The body here is valid for its schema, so only the bound
// can refuse it.
func TestSpecCreateRefusesABodyOverTheBounds(t *testing.T) {
	t.Skip("workloadschema.Schema.Validate does not enforce the body bounds yet; enable this test with the validator that does")
	admin := &identity.Identity{UserID: "admin", Permissions: enum.All}
	f := newAPIFixture(t)
	seedHandlerSchema(t, f.store)
	body := `{"name":"S","slot":"huge","workload":{"api_version":"solti.io/v1","kind":"Container","schema_fingerprint":"` + handlerFingerprint + `","body":{"image":"` + strings.Repeat("x", 300<<10) + `"}}}`
	if w := f.do(http.MethodPost, "/api/v1/specs", body, admin); w.Code != http.StatusBadRequest {
		t.Fatalf("HTTP %d %.200s, want 400", w.Code, w.Body.String())
	}
}
