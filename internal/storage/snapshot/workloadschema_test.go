package snapshot_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

func schemaEntry(t *testing.T, fingerprint string) *model.WorkloadSchema {
	t.Helper()
	w, err := model.NewWorkloadSchema(fingerprint, "solti.io/v1", "Subprocess", []byte(`{"type":"object"}`), "agent-1", time.Unix(1000, 7))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestSnapshotCarriesWorkloadSchemas(t *testing.T) {
	a, b := schemaEntry(t, strings.Repeat("a", 64)), schemaEntry(t, strings.Repeat("b", 64))
	data, err := snapshot.Marshal(inmemory.SnapshotContent{WorkloadSchemas: []*model.WorkloadSchema{b, a}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := snapshot.Unmarshal(data)
	if err != nil || len(out.WorkloadSchemas) != 2 {
		t.Fatalf("round trip: %v, %d schemas", err, len(out.WorkloadSchemas))
	}
	for i, want := range []*model.WorkloadSchema{a, b} {
		got := out.WorkloadSchemas[i]
		if got.ID() != want.ID() || got.APIVersion() != want.APIVersion() || got.Kind() != want.Kind() ||
			!bytes.Equal(got.Schema(), want.Schema()) || got.FirstAgentID() != want.FirstAgentID() || !got.FirstSeenAt().Equal(want.FirstSeenAt()) {
			t.Errorf("schema %d lost fields: %+v", i, got)
		}
	}

	// The same snapshot at the version a podium without the section wrote has
	// the section's messages ignored: the section is additive.
	again, err := snapshot.Marshal(out)
	if err != nil || !bytes.Equal(again, data) {
		t.Fatalf("a restored snapshot encodes differently: %v", err)
	}

	for name, change := range map[string]func(*raftv1.Snapshot){
		"duplicate":       func(m *raftv1.Snapshot) { m.WorkloadSchemas = append(m.WorkloadSchemas, m.WorkloadSchemas[0]) },
		"bad fingerprint": func(m *raftv1.Snapshot) { m.WorkloadSchemas[0].Fingerprint = "forged" },
		"empty schema":    func(m *raftv1.Snapshot) { m.WorkloadSchemas[0].Schema = nil },
		"no time":         func(m *raftv1.Snapshot) { m.WorkloadSchemas[0].FirstSeenAtNs = 0 },
	} {
		var m raftv1.Snapshot
		if err := proto.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		change(&m)
		bad, _ := proto.Marshal(&m)
		if _, err := snapshot.Unmarshal(bad); err == nil {
			t.Errorf("%s: invalid snapshot accepted", name)
		}
	}

	// A snapshot without the section, as every one written before it, restores
	// an empty catalog.
	old, err := snapshot.Marshal(inmemory.SnapshotContent{})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := snapshot.Unmarshal(old); err != nil || len(out.WorkloadSchemas) != 0 {
		t.Fatalf("a snapshot without schemas: %v, %d", err, len(out.WorkloadSchemas))
	}
}

func TestWorkloadSchemaJSONRoundTrip(t *testing.T) {
	w := schemaEntry(t, strings.Repeat("c", 64))
	body, err := snapshot.MarshalWorkloadSchemaJSON(w)
	if err != nil {
		t.Fatal(err)
	}
	got, err := snapshot.UnmarshalWorkloadSchemaJSON(body)
	if err != nil || got.ID() != w.ID() || !bytes.Equal(got.Schema(), w.Schema()) || !got.FirstSeenAt().Equal(w.FirstSeenAt()) {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if _, err := snapshot.MarshalWorkloadSchemaJSON(nil); err == nil {
		t.Error("nil schema encoded")
	}
	if _, err := snapshot.UnmarshalWorkloadSchemaJSON([]byte(`{"fingerprint":"x"}`)); err == nil {
		t.Error("invalid body decoded")
	}
}
