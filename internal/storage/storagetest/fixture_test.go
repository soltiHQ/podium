package storagetest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

func seeded(t *testing.T, f Fixture) *inmemory.Store {
	t.Helper()
	store := inmemory.New()
	if err := Seed(context.Background(), store, f); err != nil {
		t.Fatal(err)
	}
	return store
}

func snapshotOf(t *testing.T, store *inmemory.Store) []byte {
	t.Helper()
	data, err := snapshot.Marshal(store.SnapshotForRaft())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The benchmarks and their recorded baselines mean something only while the
// fixture keeps its shape.
func TestSeedBuildsTheDocumentedShape(t *testing.T) {
	ctx := context.Background()
	const agents, history = 45, 30
	store := seeded(t, Fixture{Agents: agents, HistoryRows: history})

	rows, err := storage.AllAgents(ctx, store, nil)
	if err != nil || len(rows) != agents {
		t.Fatalf("agents = %d, %v, want %d", len(rows), err, agents)
	}
	for i, a := range rows {
		if a.ID() != AgentID(i) {
			t.Fatalf("listing position %d holds %s, want %s", i, a.ID(), AgentID(i))
		}
		if got := len(a.MetadataAll()); got != 6 {
			t.Fatalf("%s: %d metadata entries, want 6", a.ID(), got)
		}
		if got := len(a.LabelsAll()); got != 3 {
			t.Fatalf("%s: %d labels, want 3", a.ID(), got)
		}
		if got := len(a.Capabilities()); got != 2 {
			t.Fatalf("%s: %d runners, want 2", a.ID(), got)
		}
		if !a.LastSeenAt().Equal(Epoch) || !a.StaleAt().Equal(Epoch.Add(HeartbeatInterval)) {
			t.Fatalf("%s: seen %v, stale at %v", a.ID(), a.LastSeenAt(), a.StaleAt())
		}
		cred, err := store.GetAgentCredential(ctx, a.ID())
		if err != nil || cred.Token() != AgentToken(i) {
			t.Fatalf("%s: credential %v, %v", a.ID(), cred, err)
		}
		ros, err := storage.AllRollouts(ctx, store, store.BuildRolloutFilter(storage.RolloutQueryCriteria{AgentID: a.ID()}))
		if err != nil || len(ros) != RolloutsPerAgent {
			t.Fatalf("%s: %d rollouts, %v, want %d", a.ID(), len(ros), err, RolloutsPerAgent)
		}
	}

	specs, err := storage.AllSpecs(ctx, store, nil)
	if err != nil || len(specs) != SpecCount {
		t.Fatalf("specs = %d, %v, want %d", len(specs), err, SpecCount)
	}
	fleet, err := store.GetSpec(ctx, FleetSpec)
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet.Targets()) != 0 || len(fleet.TargetLabels()) == 0 {
		t.Fatalf("%s: targets %v, labels %v, want a label selector", FleetSpec, fleet.Targets(), fleet.TargetLabels())
	}
	for _, a := range rows {
		for k, v := range fleet.TargetLabels() {
			if got, ok := a.Label(k); !ok || got != v {
				t.Fatalf("%s: %s does not carry %s=%s", FleetSpec, a.ID(), k, v)
			}
		}
	}
	explicit, err := store.GetSpec(ctx, ExplicitSpec)
	if err != nil {
		t.Fatal(err)
	}
	if got := explicit.Targets(); len(got) != ExplicitTargets || got[0] != AgentID(0) || len(explicit.TargetLabels()) != 0 {
		t.Fatalf("%s: targets %v, labels %v", ExplicitSpec, got, explicit.TargetLabels())
	}

	runs, err := storage.AllExecutions(ctx, store, nil)
	if err != nil || len(runs) != history {
		t.Fatalf("history rows = %d, %v, want %d", len(runs), err, history)
	}
}

func TestSeedHistoryOnly(t *testing.T) {
	ctx := context.Background()
	store := seeded(t, Fixture{HistoryRows: 12})
	if rows, err := storage.AllAgents(ctx, store, nil); err != nil || len(rows) != 0 {
		t.Fatalf("agents = %d, %v, want none", len(rows), err)
	}
	if runs, err := storage.AllExecutions(ctx, store, nil); err != nil || len(runs) != 12 {
		t.Fatalf("history rows = %d, %v, want 12", len(runs), err)
	}
}

func TestSeedRejectsNegativeSizes(t *testing.T) {
	for _, f := range []Fixture{{Agents: -1}, {HistoryRows: -1}} {
		if err := Seed(context.Background(), inmemory.New(), f); !errors.Is(err, storage.ErrInvalidArgument) {
			t.Fatalf("Seed(%+v) = %v, want ErrInvalidArgument", f, err)
		}
	}
}

// No timestamp comes from the wall clock, so equal fixtures encode to equal bytes.
func TestSeedIsDeterministic(t *testing.T) {
	f := Fixture{Agents: 30, HistoryRows: 30}
	if a, b := snapshotOf(t, seeded(t, f)), snapshotOf(t, seeded(t, f)); !bytes.Equal(a, b) {
		t.Fatal("two seeds of one fixture encode differently")
	}
}

// Fixture's doc comment quotes about 900 B per agent in a snapshot, the shape
// the benchmarks' recorded baselines were measured on.
func TestSeedAgentSize(t *testing.T) {
	small := len(snapshotOf(t, seeded(t, Fixture{Agents: 100})))
	large := len(snapshotOf(t, seeded(t, Fixture{Agents: 1000})))
	if perAgent := (large - small) / 900; perAgent < 810 || perAgent > 990 {
		t.Fatalf("an agent takes %d B in a snapshot, want about 900", perAgent)
	}
}
