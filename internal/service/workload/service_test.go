package workload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	for _, tc := range []struct {
		name     string
		declared []model.RunnerCapability
		offered  OfferedSchema
		wanted   []string
	}{
		{"undeclared", declares(other), OfferedSchema{good, []byte(validSchema)}, []string{other}},
		{"mismatched", declares(other), OfferedSchema{other, []byte(validSchema)}, []string{other}},
		{"not JSON", declares(other), OfferedSchema{other, []byte(`{`)}, []string{other}},
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

// countingStore counts the writes and the reads of the catalog that reach the store.
type countingStore struct {
	storage.Storage
	writes, reads atomic.Int64
}

func (c *countingStore) UpsertWorkloadSchema(ctx context.Context, s *model.WorkloadSchema) error {
	c.writes.Add(1)
	return c.Storage.UpsertWorkloadSchema(ctx, s)
}

func (c *countingStore) GetWorkloadSchema(ctx context.Context, fp string) (*model.WorkloadSchema, error) {
	c.reads.Add(1)
	return c.Storage.GetWorkloadSchema(ctx, fp)
}

// The catalog already holds the schema (another node, or an earlier process):
// 1000 exchanges that re-offer it, with an undeclared schema and garbage for
// company, write nothing and read the store once.
func TestExchangeWritesNothingWithoutNewSchemas(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{Storage: inmemory.New()}
	fp := fingerprintOf(t, validSchema)
	seed, err := model.NewWorkloadSchema(fp, "solti.io/v1", "KindA", []byte(validSchema), "seeder", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Storage.UpsertWorkloadSchema(ctx, seed); err != nil {
		t.Fatal(err)
	}
	svc := New(store, zerolog.Nop())
	other := fingerprintOf(t, `{"type":"string"}`)
	offered := []OfferedSchema{
		{fp, []byte(validSchema)},
		{other, []byte(`{"type":"string"}`)}, // not declared
		{fp, []byte(`garbage`)},
		{strings.Repeat("e", 64), []byte(`garbage`)},
	}
	for range 1000 {
		if got := svc.Exchange(ctx, "agent-1", declares(fp), offered); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	}
	if n := store.writes.Load(); n != 0 {
		t.Fatalf("1000 exchanges wrote %d times, want 0", n)
	}
	if n := store.reads.Load(); n > 1 {
		t.Fatalf("1000 exchanges of a known fingerprint read the store %d times, want at most 1", n)
	}
}

func TestExchangeLogsARepeatedOfferOnce(t *testing.T) {
	svc, _, logs := newService(t)
	o := OfferedSchema{strings.Repeat("f", 64), []byte(`x`)}
	svc.Exchange(context.Background(), "agent-1", nil, []OfferedSchema{o, o, o, o})
	if n := strings.Count(logs.String(), "rejected"); n != 1 {
		t.Fatalf("%d log lines for one repeated offer, want 1", n)
	}
}

// failingStore fails every lookup and every write.
type failingStore struct{ storage.Storage }

func (failingStore) GetWorkloadSchema(context.Context, string) (*model.WorkloadSchema, error) {
	return nil, errors.New("store down")
}

func (failingStore) UpsertWorkloadSchema(context.Context, *model.WorkloadSchema) error {
	return errors.New("store down")
}

func TestExchangeLogsALookupFailureOncePerFingerprint(t *testing.T) {
	logs := &bytes.Buffer{}
	svc := New(failingStore{inmemory.New()}, zerolog.New(logs))
	fp := fingerprintOf(t, validSchema)
	got := svc.Exchange(context.Background(), "agent-1", declares(fp), []OfferedSchema{{fp, []byte(validSchema)}})
	if !slices.Equal(got, []string{fp}) {
		t.Fatalf("a failed lookup must stay wanted, got %v", got)
	}
	if n := strings.Count(logs.String(), "lookup failed"); n != 1 {
		t.Fatalf("%d lookup failure lines, want 1", n)
	}
}

// brokenSchema is JSON whose fingerprint is its own, which Compile refuses.
const brokenSchema = `{"type":"nonsense"}`

// logLines decodes the JSON log lines in logs.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// A schema whose fingerprint is right and that Compile refuses is refused for
// good by this binary: it is no longer wanted, from the sync that offered it on,
// its fingerprint costs no store read, and the refusal is logged once, with the
// reason in a field, however often an agent offers it again.
func TestExchangeRemembersARefusedSchema(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{Storage: inmemory.New()}
	logs := &bytes.Buffer{}
	svc := New(store, zerolog.New(logs))
	fp := fingerprintOf(t, brokenSchema)
	offer := []OfferedSchema{{fp, []byte(brokenSchema)}}
	if got := svc.Exchange(ctx, "agent-1", declares(fp), nil); !slices.Equal(got, []string{fp}) {
		t.Fatalf("an unknown fingerprint: wanted %v, want [%s]", got, fp)
	}
	if got := svc.Exchange(ctx, "agent-1", declares(fp), offer); len(got) != 0 {
		t.Fatalf("the sync that offered a refused schema wants %v", got)
	}
	reads := store.reads.Load()
	for range 100 {
		if got := svc.Exchange(ctx, "agent-2", declares(fp), nil); len(got) != 0 {
			t.Fatalf("a refused schema is wanted again: %v", got)
		}
		svc.Exchange(ctx, "agent-3", declares(fp), offer)
	}
	if n := store.reads.Load() - reads; n != 0 {
		t.Fatalf("200 exchanges of a refused fingerprint read the store %d times, want 0", n)
	}
	if n := store.writes.Load(); n != 0 {
		t.Fatalf("a refused schema was stored %d times", n)
	}
	var refusals []map[string]any
	for _, line := range logLines(t, logs) {
		if line["fingerprint"] == fp {
			refusals = append(refusals, line)
		}
	}
	if len(refusals) != 1 || refusals[0]["level"] != "warn" {
		t.Fatalf("%d log lines name the refused fingerprint, want one warning: %v", len(refusals), refusals)
	}
	reason, _ := refusals[0]["reason"].(string)
	if !strings.Contains(reason, "nonsense") || strings.Contains(refusals[0]["message"].(string), "nonsense") {
		t.Fatalf("the refusal keeps the reason out of its field: %v", refusals[0])
	}
	_, ok, err := svc.Compiled(ctx, fp)
	if ok || err == nil || !strings.Contains(err.Error(), "podium refused") || !strings.Contains(err.Error(), "nonsense") {
		t.Fatalf("Compiled of a refused fingerprint: %v %v, want a refusal with its reason", ok, err)
	}
}

// A schema in the catalog that this binary cannot compile, which another
// version stored, is refused like one offered: Compiled says why, and asks the
// store once.
func TestCompiledRefusesAStoredSchemaThatDoesNotCompile(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{Storage: inmemory.New()}
	logs := &bytes.Buffer{}
	fp := fingerprintOf(t, brokenSchema)
	entry, err := model.NewWorkloadSchema(fp, "solti.io/v1", "KindA", []byte(brokenSchema), "older-podium", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Storage.UpsertWorkloadSchema(ctx, entry); err != nil {
		t.Fatal(err)
	}
	svc := New(store, zerolog.New(logs))
	for range 3 {
		_, ok, err := svc.Compiled(ctx, fp)
		if ok || err == nil || !strings.Contains(err.Error(), "podium refused") || !strings.Contains(err.Error(), fp) || !strings.Contains(err.Error(), "nonsense") {
			t.Fatalf("Compiled = %v %v, want a refusal that names the fingerprint and the reason", ok, err)
		}
	}
	if n := store.reads.Load(); n != 1 {
		t.Fatalf("three Compiled calls read the store %d times, want 1", n)
	}
	if n := strings.Count(logs.String(), `"level":"warn"`); n != 1 {
		t.Fatalf("%d warnings for one refused schema, want 1: %s", n, logs.String())
	}
}

// The refusals kept are bounded: once 4096 fingerprints are refused, a new one
// is not remembered, and stays wanted, as before; the ones remembered stay
// refused.
func TestRefusedSchemasAreBounded(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newService(t)
	broken := func(i int) (string, []byte) {
		raw := []byte(fmt.Sprintf(`{"type":"nonsense-%d"}`, i))
		return fingerprintOf(t, string(raw)), raw
	}
	const limit = 4096
	for from := 0; from < limit; from += 256 {
		var fps []string
		var offer []OfferedSchema
		for i := from; i < from+256; i++ {
			fp, raw := broken(i)
			fps = append(fps, fp)
			offer = append(offer, OfferedSchema{fp, raw})
		}
		if got := svc.Exchange(ctx, "agent-1", declares(fps...), offer); len(got) != 0 {
			t.Fatalf("batch at %d: %d refused schemas are wanted", from, len(got))
		}
	}
	fp, raw := broken(limit)
	if got := svc.Exchange(ctx, "agent-1", declares(fp), []OfferedSchema{{fp, raw}}); !slices.Equal(got, []string{fp}) {
		t.Fatalf("a refusal past the bound: wanted %v, want it asked for again", got)
	}
	first, _ := broken(0)
	if got := svc.Exchange(ctx, "agent-1", declares(first), nil); len(got) != 0 {
		t.Fatalf("a remembered refusal is wanted again: %v", got)
	}
}

// What an agent sends reaches the logs only in fields, and cut: the offered
// fingerprint and the reason a schema is refused, which may quote the schema's
// regular expressions, so a log line in the text format cannot be forged.
func TestExchangeLogsAgentTextInCappedFields(t *testing.T) {
	svc, _, logs := newService(t)
	forged := "\n{\"level\":\"error\",\"message\":\"forged\"}\n"
	pattern := `{"type":"object","properties":{"a":{"type":"string","pattern":"(` + strings.Repeat("x", 4<<10) + `\n{\"level\":\"error\"}"}}}`
	patternFP := fingerprintOf(t, pattern)
	svc.Exchange(context.Background(), "agent-1", declares(patternFP), []OfferedSchema{
		{strings.Repeat("f", 10<<10) + forged, []byte(`{}`)}, // not declared
		{patternFP, []byte(pattern)},                         // does not compile
	})
	lines := logLines(t, logs)
	if len(lines) != 2 {
		t.Fatalf("%d log lines, want 2: %s", len(lines), logs.String())
	}
	for _, line := range lines {
		message, _ := line["message"].(string)
		if strings.Contains(message, "forged") || strings.Contains(message, "level") || strings.Contains(message, "xxxx") {
			t.Fatalf("the message carries agent text: %q", message)
		}
		for field, limit := range map[string]int{"fingerprint": 128 + len("…"), "reason": 512 + len("…")} {
			if v, _ := line[field].(string); len(v) > limit {
				t.Fatalf("the %s field holds %d bytes, want at most %d", field, len(v), limit)
			}
		}
	}
}

// The catalog view says why a fingerprint is refused, and nothing for one that
// is not.
func TestTypesShowARefusal(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newService(t)
	refused := fingerprintOf(t, brokenSchema)
	good := fingerprintOf(t, validSchema)
	declared := declares(refused, good)
	a, err := model.NewAgent("agent-1", "agent-1", "http://agent-1:1")
	if err != nil {
		t.Fatal(err)
	}
	a.SetCapabilities(declared)
	if err = store.UpsertAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	svc.Exchange(ctx, "agent-1", declared, []OfferedSchema{{refused, []byte(brokenSchema)}, {good, []byte(validSchema)}})
	types, err := svc.Types(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]FingerprintView{}
	for _, ty := range types {
		for _, f := range ty.Fingerprints {
			got[f.Fingerprint] = f
		}
	}
	if f := got[refused]; f.Schema != nil || !strings.Contains(f.Refused, "nonsense") {
		t.Fatalf("the refused fingerprint shows as %+v", f)
	}
	if f := got[good]; f.Schema == nil || f.Refused != "" {
		t.Fatalf("the accepted fingerprint shows as %+v", f)
	}
}
