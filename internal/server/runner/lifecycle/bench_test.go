package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// benchStore opens an in-memory store and seeds it.
func benchStore(b *testing.B, f storagetest.Fixture) *inmemory.Store {
	b.Helper()
	s := inmemory.New()
	if err := storagetest.Seed(context.Background(), s, f); err != nil {
		b.Fatal(err)
	}
	return s
}

// silentAgents leaves the first n agents unseen for ten minutes at now, and
// disconnected: past their staleness deadline, so a tick scans them, yet short
// of the delete threshold and no longer worsening, so nothing is due.
func silentAgents(ctx context.Context, s storage.Storage, n int, now time.Time) error {
	if n == 0 {
		return nil
	}
	return s.WithTx(ctx, func(tx storage.Storage) error {
		seen := now.Add(-10 * time.Minute)
		for i := range n {
			a, err := tx.GetAgent(ctx, storagetest.AgentID(i))
			if err != nil {
				return err
			}
			a.SetLastSeenAt(seen)
			a.SetStaleAt(seen.Add(storagetest.HeartbeatInterval))
			a.SetStatus(enum.AgentStatusDisconnected)
			if err = tx.UpsertAgent(ctx, a); err != nil {
				return err
			}
		}
		return nil
	})
}

// BenchmarkLifecycleTick is one tick over 5 000 agents when some share of them
// is past its staleness deadline. A silent agent stays in the scan until it is
// deleted, so this is the steady cost of a fleet with dead members. Nothing is
// due, so a tick opens no transaction: only the scan is measured.
func BenchmarkLifecycleTick(b *testing.B) {
	const agents = 5000
	for _, tc := range []struct {
		name  string
		stale int // percent of the agents past their staleness deadline
	}{
		{"stale0", 0},
		{"stale10pct", 10},
		{"stale100pct", 100},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ctx := context.Background()
			store := benchStore(b, storagetest.Fixture{Agents: agents})
			// The clock is pinned inside the heartbeat deadline of the fixture.
			now := storagetest.Epoch.Add(10 * time.Second)
			stale := agents * tc.stale / 100
			if err := silentAgents(ctx, store, stale, now); err != nil {
				b.Fatal(err)
			}
			scanned, err := storage.AllAgents(ctx, store, inmemory.NewAgentFilter().StaleAtBefore(now))
			if err != nil || len(scanned) != stale {
				b.Fatalf("a tick scans %d agents, %v, want %d", len(scanned), err, stale)
			}

			hub := event.NewHub(zerolog.Nop())
			b.Cleanup(hub.Close)
			counter := &txCounter{Storage: store}
			r := &Runner{
				store:  counter,
				cfg:    Config{}.withDefaults(),
				logger: zerolog.Nop(),
				hub:    hub,
				clock:  func() time.Time { return now },
			}
			b.ReportAllocs()
			for b.Loop() {
				r.tick(ctx)
			}
			if got := counter.txs.Load(); got != 0 {
				b.Fatalf("the ticks opened %d transactions, want a scan only", got)
			}
		})
	}
}
