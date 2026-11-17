package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
)

const (
	catalogSchema = `{"type":"object","properties":{"cmd":{"type":"string"}}}`
	fpReceived    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fpPending     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func (f *apiFixture) declaringAgent(t *testing.T, id string, gvks ...model.GroupVersionKind) {
	t.Helper()
	a, err := model.NewAgent(id, id, "http://"+id+":1")
	if err != nil {
		t.Fatal(err)
	}
	a.SetCapabilities([]model.RunnerCapability{{Name: "r", Workloads: gvks}})
	if err := f.store.UpsertAgent(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func (f *apiFixture) catalogSchema(t *testing.T, fp, apiVersion, kind, schema string) {
	t.Helper()
	e, err := model.NewWorkloadSchema(fp, apiVersion, kind, []byte(schema), "agent-1", time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpsertWorkloadSchema(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func specsReader(f *apiFixture, t *testing.T) *identity.Identity {
	return &identity.Identity{UserID: "reader", Permissions: []enum.Permission{enum.SpecsGet}}
}

func TestWorkloadTypesListsDeclaredTypesWithTheirAgents(t *testing.T) {
	f := newAPIFixture(t)
	f.declaringAgent(t, "agent-2", model.GroupVersionKind{Group: "solti.io", Version: "v1", Kind: "Wasm", SchemaFingerprint: fpReceived})
	f.declaringAgent(t, "agent-1",
		model.GroupVersionKind{Group: "solti.io", Version: "v1", Kind: "Wasm", SchemaFingerprint: fpReceived},
		model.GroupVersionKind{Group: "solti.io", Version: "v1", Kind: "Container", SchemaFingerprint: fpPending},
	)
	f.catalogSchema(t, fpReceived, "solti.io/v1", "Wasm", catalogSchema)

	w := f.do(http.MethodGet, "/api/v1/workload-types", "", specsReader(f, t))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var got []restv1.WorkloadType
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, w.Body.String())
	}
	want := `[{"api_version":"solti.io/v1","kind":"Container","fingerprints":[{"fingerprint":"` + fpPending + `","agents":["agent-1"],"received":false}]},` +
		`{"api_version":"solti.io/v1","kind":"Wasm","fingerprints":[{"fingerprint":"` + fpReceived + `","agents":["agent-1","agent-2"],"received":true}]}]`
	if strings.Contains(w.Body.String(), "schema\"") {
		t.Fatalf("the list carries a schema: %s", w.Body.String())
	}
	gotJSON, _ := json.Marshal(got)
	if string(gotJSON) != want {
		t.Fatalf("got  %s\nwant %s", gotJSON, want)
	}
}

func TestWorkloadTypeDetailCarriesTheSchema(t *testing.T) {
	f := newAPIFixture(t)
	f.declaringAgent(t, "agent-1",
		model.GroupVersionKind{Group: "example.solti.io", Version: "v1", Kind: "Wasm", SchemaFingerprint: fpReceived},
		model.GroupVersionKind{Group: "example.solti.io", Version: "v1", Kind: "Pending", SchemaFingerprint: fpPending},
	)
	f.catalogSchema(t, fpReceived, "example.solti.io/v1", "Wasm", catalogSchema)

	w := f.do(http.MethodGet, "/api/v1/workload-types/example.solti.io/v1/Wasm", "", specsReader(f, t))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var got struct {
		APIVersion   string `json:"api_version"`
		Kind         string `json:"kind"`
		Fingerprints []struct {
			Fingerprint string          `json:"fingerprint"`
			Agents      []string        `json:"agents"`
			Received    bool            `json:"received"`
			Schema      json.RawMessage `json:"schema"`
		} `json:"fingerprints"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.APIVersion != "example.solti.io/v1" || got.Kind != "Wasm" || len(got.Fingerprints) != 1 {
		t.Fatalf("got %s", w.Body.String())
	}
	fp := got.Fingerprints[0]
	if fp.Fingerprint != fpReceived || !fp.Received || string(fp.Schema) != catalogSchema {
		t.Fatalf("fingerprint %+v (schema %s)", fp, fp.Schema)
	}

	w = f.do(http.MethodGet, "/api/v1/workload-types/example.solti.io/v1/Pending", "", specsReader(f, t))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"schema"`) || !strings.Contains(w.Body.String(), `"received":false`) {
		t.Fatalf("pending type: %d %s", w.Code, w.Body.String())
	}

	for _, target := range []string{
		"/api/v1/workload-types/example.solti.io/v1/Unknown",
		"/api/v1/workload-types/example.solti.io/v2/Wasm",
		"/api/v1/workload-types/example.solti.io/v1",
		"/api/v1/workload-types/example.solti.io/v1/Wasm/extra",
		"/api/v1/workload-types/",
	} {
		if w := f.do(http.MethodGet, target, "", specsReader(f, t)); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, w.Code)
		}
	}
}

func TestWorkloadTypesNeedSpecsGet(t *testing.T) {
	f := newAPIFixture(t)
	f.declaringAgent(t, "agent-1", model.GroupVersionKind{Group: "solti.io", Version: "v1", Kind: "Wasm", SchemaFingerprint: fpReceived})
	f.catalogSchema(t, fpReceived, "solti.io/v1", "Wasm", catalogSchema)
	for _, target := range []string{"/api/v1/workload-types", "/api/v1/workload-types/solti.io/v1/Wasm"} {
		if w := f.do(http.MethodGet, target, "", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s anonymous: %d", target, w.Code)
		}
		if w := f.do(http.MethodGet, target, "", &identity.Identity{UserID: "x", Permissions: allExcept(enum.SpecsGet)}); w.Code != http.StatusForbidden {
			t.Errorf("%s without specs.get: %d", target, w.Code)
		}
		if w := f.do(http.MethodGet, target, "", specsReader(f, t)); w.Code != http.StatusOK {
			t.Errorf("%s with specs.get: %d", target, w.Code)
		}
	}
}
