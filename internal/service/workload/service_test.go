package workload

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/workloadschema"
)

const validSchema = `{"type":"object","properties":{"cmd":{"type":"string"}}}`

func fingerprintOf(t *testing.T, raw string) string {
	t.Helper()
	fp, err := workloadschema.Fingerprint([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func declares(fps ...string) []model.RunnerCapability {
	var w []model.GroupVersionKind
	for i, fp := range fps {
		w = append(w, model.GroupVersionKind{Group: "solti.io", Version: "v1", Kind: "Kind" + string(rune('A'+i)), SchemaFingerprint: fp})
	}
	return []model.RunnerCapability{{Name: "default", Workloads: w}}
}

func newService(t *testing.T) (*Service, storage.Storage, *bytes.Buffer) {
	t.Helper()
	store := inmemory.New()
	logs := &bytes.Buffer{}
	return New(store, zerolog.New(logs)), store, logs
}

func TestExchangeWantsUnknownFingerprints(t *testing.T) {
	svc, _, _ := newService(t)
	b, a := strings.Repeat("b", 64), strings.Repeat("a", 64)
	got := svc.Exchange(context.Background(), "agent-1", append(declares(b, a, ""), declares(b)...), nil)
	if want := []string{a, b}; !slices.Equal(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestExchangeStoresADeclaredSchema(t *testing.T) {
	svc, store, _ := newService(t)
	ctx := context.Background()
	fp := fingerprintOf(t, validSchema)
	got := svc.Exchange(ctx, "agent-1", declares(fp), []OfferedSchema{{Fingerprint: fp, Schema: []byte(validSchema)}})
	if len(got) != 0 {
		t.Fatalf("a stored schema is still wanted: %v", got)
	}
	e, err := store.GetWorkloadSchema(ctx, fp)
	if err != nil {
		t.Fatal(err)
	}
	if e.APIVersion() != "solti.io/v1" || e.Kind() != "KindA" || e.FirstAgentID() != "agent-1" {
		t.Fatalf("entry %s %s by %s", e.APIVersion(), e.Kind(), e.FirstAgentID())
	}
	if _, ok, err := svc.Compiled(ctx, fp); err != nil || !ok {
		t.Fatalf("Compiled: %v %v", ok, err)
	}
	if _, ok, err := svc.Compiled(ctx, strings.Repeat("c", 64)); err != nil || ok {
		t.Fatalf("Compiled of an unknown fingerprint: %v %v", ok, err)
	}
}

func TestExchangeRejectsAnUndeclaredOrMismatchedSchema(t *testing.T) {
	good := fingerprintOf(t, validSchema)
	other := fingerprintOf(t, `{"type":"string"}`)
	broken := `{"type":"nonsense"}`
	brokenFP := fingerprintOf(t, broken)
	for _, tc := range []struct {
		name     string
		declared []model.RunnerCapability
		offered  OfferedSchema
		wanted   []string
	}{
		{"undeclared", declares(other), OfferedSchema{good, []byte(validSchema)}, []string{other}},
		{"mismatched", declares(other), OfferedSchema{other, []byte(validSchema)}, []string{other}},
		{"not JSON", declares(other), OfferedSchema{other, []byte(`{`)}, []string{other}},
		{"does not compile", declares(brokenFP), OfferedSchema{brokenFP, []byte(broken)}, []string{brokenFP}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, logs := newService(t)
			got := svc.Exchange(context.Background(), "agent-7", tc.declared, []OfferedSchema{tc.offered})
			if !slices.Equal(got, tc.wanted) {
				t.Fatalf("want %v, got %v", tc.wanted, got)
			}
			if !strings.Contains(logs.String(), `"agent_id":"agent-7"`) || !strings.Contains(logs.String(), "rejected") {
				t.Fatalf("log: %q", logs.String())
			}
			res, err := store.ListWorkloadSchemas(context.Background(), storage.ListOptions{})
			if err != nil || len(res.Items) != 0 {
				t.Fatalf("store holds %v %v", res, err)
			}
		})
	}
}

func TestExchangeRejectsAnOversizedSchema(t *testing.T) {
	svc, store, logs := newService(t)
	big := `{"description":"` + strings.Repeat("x", workloadschema.MaxSchemaBytes) + `"}`
	fp := strings.Repeat("d", 64)
	got := svc.Exchange(context.Background(), "agent-1", declares(fp), []OfferedSchema{{fp, []byte(big)}})
	if !slices.Equal(got, []string{fp}) {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(logs.String(), "agent-1") || !strings.Contains(logs.String(), "size limit") {
		t.Fatalf("log: %q", logs.String())
	}
	if _, err := store.GetWorkloadSchema(context.Background(), fp); err == nil {
		t.Fatal("oversized schema was stored")
	}
}

func TestExchangeKeepsTheFirstAgent(t *testing.T) {
	svc, store, _ := newService(t)
	ctx := context.Background()
	fp := fingerprintOf(t, validSchema)
	offer := []OfferedSchema{{fp, []byte(validSchema)}}
	svc.Exchange(ctx, "first", declares(fp), offer)
	other := []model.RunnerCapability{{Workloads: []model.GroupVersionKind{{Version: "v2", Kind: "Other", SchemaFingerprint: fp}}}}
	if got := svc.Exchange(ctx, "second", other, offer); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	e, err := store.GetWorkloadSchema(ctx, fp)
	if err != nil {
		t.Fatal(err)
	}
	if e.FirstAgentID() != "first" || e.Kind() != "KindA" {
		t.Fatalf("entry by %s kind %s", e.FirstAgentID(), e.Kind())
	}
}

// countingStore counts the writes that reach the store.
type countingStore struct {
	storage.Storage
	writes atomic.Int64
}

func (c *countingStore) UpsertWorkloadSchema(ctx context.Context, s *model.WorkloadSchema) error {
	c.writes.Add(1)
	return c.Storage.UpsertWorkloadSchema(ctx, s)
}

func TestExchangeWritesNothingWithoutNewSchemas(t *testing.T) {
	ctx := context.Background()
	inner := inmemory.New()
	store := &countingStore{Storage: inner}
	svc := New(store, zerolog.Nop())
	fp := fingerprintOf(t, validSchema)
	svc.Exchange(ctx, "agent-1", declares(fp), []OfferedSchema{{fp, []byte(validSchema)}})
	if store.writes.Load() != 1 {
		t.Fatalf("setup wrote %d", store.writes.Load())
	}
	for range 1000 {
		if got := svc.Exchange(ctx, "agent-1", declares(fp), nil); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	}
	if n := store.writes.Load(); n != 1 {
		t.Fatalf("1000 exchanges wrote %d times in all, want only the setup write", n)
	}
}
