package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/buildinfo"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// agents5000 is a store of 5000 agents of every status, each with a rollout.
func agents5000(b *testing.B) *inmemory.Store {
	b.Helper()
	ctx := context.Background()
	s := inmemory.New()
	statuses := []enum.AgentStatus{enum.AgentStatusActive, enum.AgentStatusInactive, enum.AgentStatusDisconnected}
	for i := range 5000 {
		a, err := model.NewAgentFrom(model.AgentParams{
			ID: fmt.Sprintf("agent-%05d", i), Name: "agent", Endpoint: "http://10.0.0.1:8085",
			EndpointType: 2, APIVersion: 1, OS: "linux", Arch: "amd64", Platform: "x86_64-linux",
			Metadata: map[string]string{"zone": "a", "rack": "12"},
		})
		if err != nil {
			b.Fatal(err)
		}
		a.MarkStatus(statuses[i%len(statuses)])
		if err = s.UpsertAgent(ctx, a); err != nil {
			b.Fatal(err)
		}
		r, err := model.NewRollout(fmt.Sprintf("spec-%d", i%50), a.ID(), 1)
		if err != nil {
			b.Fatal(err)
		}
		if i%3 == 0 {
			r.MarkSynced(1)
		}
		if err = s.UpsertRollout(ctx, r); err != nil {
			b.Fatal(err)
		}
	}
	return s
}

// BenchmarkScrape5000Agents is the cost of one GET /metrics over a store of 5000
// agents and 5000 rollouts: the two counts of the inventory, and the rest of the
// exposition.
func BenchmarkScrape5000Agents(b *testing.B) {
	m := New(buildinfo.Read())
	m.Inventory(agents5000(b))
	h := m.Handler()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if w.Code != http.StatusOK {
			b.Fatal(w.Code)
		}
	}
}

// BenchmarkScanInstead5000Agents is what the inventory would cost if it scanned the
// rows, which clones each one, for the same store: the counts are the cheaper of
// the two.
func BenchmarkScanInstead5000Agents(b *testing.B) {
	s := agents5000(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		agents, err := s.ScanAgents(ctx, nil)
		if err != nil {
			b.Fatal(err)
		}
		rollouts, err := s.ScanRollouts(ctx, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(agents) != 5000 || len(rollouts) != 5000 {
			b.Fatal("short scan")
		}
	}
}

// BenchmarkInventoryOnly5000Agents is the two counts alone.
func BenchmarkInventoryOnly5000Agents(b *testing.B) {
	s := agents5000(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := s.CountAgents(ctx); err != nil {
			b.Fatal(err)
		}
		if _, err := s.CountRollouts(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
