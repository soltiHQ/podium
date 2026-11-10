package inmemory

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// The counts are what a scan of the rows tallies, for a store of every status, and
// the same through the helpers that fall back to a scan.
func TestCountsMatchAScan(t *testing.T) {
	ctx := context.Background()
	s := New()
	statuses := []enum.AgentStatus{enum.AgentStatusActive, enum.AgentStatusInactive, enum.AgentStatusDisconnected}
	for i := range 30 {
		a, err := model.NewAgent(fmt.Sprintf("agent-%d", i), "n", "http://10.0.0.1:8085")
		if err != nil {
			t.Fatal(err)
		}
		a.MarkStatus(statuses[i%len(statuses)*(i%2)])
		requireNoErr(t, s.UpsertAgent(ctx, a))

		r, err := model.NewRollout(fmt.Sprintf("spec-%d", i), a.ID(), 1)
		if err != nil {
			t.Fatal(err)
		}
		switch i % 4 {
		case 1:
			r.MarkSynced(1)
		case 2:
			r.MarkDrift()
		case 3:
			r.MarkFailed("x")
		}
		requireNoErr(t, s.UpsertRollout(ctx, r))
	}

	agents, err := s.ScanAgents(ctx, nil)
	requireNoErr(t, err)
	wantAgents := map[enum.AgentStatus]int{}
	for _, a := range agents {
		wantAgents[a.Status()]++
	}
	gotAgents, err := s.CountAgents(ctx)
	requireNoErr(t, err)
	if fmt.Sprint(gotAgents) != fmt.Sprint(wantAgents) {
		t.Fatalf("CountAgents %v, want %v", gotAgents, wantAgents)
	}

	rollouts, err := s.ScanRollouts(ctx, nil)
	requireNoErr(t, err)
	wantRollouts := map[enum.SyncStatus]int{}
	for _, r := range rollouts {
		wantRollouts[r.Status()]++
	}
	gotRollouts, err := s.CountRollouts(ctx)
	requireNoErr(t, err)
	if fmt.Sprint(gotRollouts) != fmt.Sprint(wantRollouts) {
		t.Fatalf("CountRollouts %v, want %v", gotRollouts, wantRollouts)
	}

	// The helpers use the store's own count, and a scan for a store without one.
	viaHelper, err := storage.CountAgents(ctx, s)
	requireNoErr(t, err)
	viaScan, err := storage.CountAgents(ctx, noCounter{s})
	requireNoErr(t, err)
	if fmt.Sprint(viaHelper) != fmt.Sprint(wantAgents) || fmt.Sprint(viaScan) != fmt.Sprint(wantAgents) {
		t.Fatalf("CountAgents through the helper %v, through a scan %v, want %v", viaHelper, viaScan, wantAgents)
	}
	rolloutsViaScan, err := storage.CountRollouts(ctx, noCounter{s})
	requireNoErr(t, err)
	if fmt.Sprint(rolloutsViaScan) != fmt.Sprint(wantRollouts) {
		t.Fatalf("CountRollouts through a scan %v, want %v", rolloutsViaScan, wantRollouts)
	}
}

// noCounter hides the Counter capability of a store.
type noCounter struct{ storage.Storage }

// A count stops once its context is done, as a scan does.
func TestCountStopsOnceContextIsDone(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.CountAgents(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CountAgents on a done context: %v, want context.Canceled", err)
	}
	if _, err := s.CountRollouts(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CountRollouts on a done context: %v, want context.Canceled", err)
	}
}
