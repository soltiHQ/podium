package durable

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// fleetSizes are the agent counts the benchmarks run at: a small install, a
// large one and the 5 000 agents a standalone podium is meant to carry.
var fleetSizes = []int{100, 1000, 5000}

// benchStore opens a store in a fresh directory and seeds it with agents.
func benchStore(b *testing.B, agents int) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "state"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	if err = storagetest.Seed(context.Background(), s, storagetest.Fixture{Agents: agents}); err != nil {
		b.Fatal(err)
	}
	return s
}

// touchAgent moves the uptime of one agent in a transaction of its own.
func touchAgent(ctx context.Context, s storage.Storage, agents, i int) error {
	return s.WithTx(ctx, func(tx storage.Storage) error {
		a, err := tx.GetAgent(ctx, storagetest.AgentID(i%agents))
		if err != nil {
			return err
		}
		a.SetUptimeSeconds(a.UptimeSeconds() + 30)
		return tx.UpsertAgent(ctx, a)
	})
}

// BenchmarkTxNoop is a transaction that reads and writes nothing.
func BenchmarkTxNoop(b *testing.B) {
	for _, n := range fleetSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			s := benchStore(b, n)
			ctx := context.Background()
			noop := func(storage.Storage) error { return nil }
			b.ReportAllocs()
			storagetest.Measure(b, func(int) error { return s.WithTx(ctx, noop) })
		})
	}
}

// BenchmarkTxAgentUpdate is a transaction that updates one agent.
func BenchmarkTxAgentUpdate(b *testing.B) {
	for _, n := range fleetSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			s := benchStore(b, n)
			ctx := context.Background()
			b.ReportAllocs()
			storagetest.Measure(b, func(i int) error { return touchAgent(ctx, s, n, i) })
		})
	}
}

// committed is the index of the last committed log entry: the number of commits
// the store has made.
func committed(s *Store) uint64 {
	_ = s.writer.Acquire(context.Background(), 1)
	defer s.writer.Release(1)
	return s.last
}

// BenchmarkHeartbeat is the heartbeat of an enrolled agent as discovery runs
// it: verify the token, which writes nothing while it matches, read the stored
// agent, which discovery needs for its events, then upsert the agent. It
// reports the commits a heartbeat makes, and how many heartbeats tell the UI to
// refresh: none, since these change nothing an operator watches.
func BenchmarkHeartbeat(b *testing.B) {
	for _, n := range fleetSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			s := benchStore(b, n)
			svc := agent.New(s, zerolog.Nop())
			ctx := context.Background()
			first := committed(s)
			refreshes := 0
			b.ReportAllocs()
			storagetest.Measure(b, func(i int) error {
				// The payload is built per heartbeat, as discovery does. An agent
				// reports a longer uptime each time, or it would look restarted.
				p := storagetest.AgentParams(i % n)
				p.UptimeSeconds += int64(i)
				a, err := model.NewAgentFrom(p)
				if err != nil {
					return err
				}
				if err = svc.VerifyOrEnrollToken(ctx, p.ID, storagetest.AgentToken(i%n)); err != nil {
					return err
				}
				if _, err = svc.Get(ctx, p.ID); err != nil {
					return err
				}
				res, err := svc.Upsert(ctx, a)
				if res.Visible || res.Reconnected {
					refreshes++
				}
				return err
			})
			b.ReportMetric(float64(committed(s)-first)/float64(b.N), "commits/op")
			b.ReportMetric(float64(refreshes)/float64(b.N), "refreshes/op")
		})
	}
}

// BenchmarkSnapshot is the snapshot the store writes when its log reaches the
// threshold: it encodes the published state and writes it, and truncates the log,
// in one commit, while writers wait. Before each one, untimed, a one-agent update
// commits an entry to take it at.
func BenchmarkSnapshot(b *testing.B) {
	for _, n := range fleetSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			s := benchStore(b, n)
			ctx := context.Background()
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				b.StopTimer()
				if err := touchAgent(ctx, s, n, i); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := s.writeSnapshot(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReadDuringWrite is the latency of GetAgent while another goroutine
// commits one-agent updates back to back. It reports no allocations: the
// counters are process wide and would include the writer's.
func BenchmarkReadDuringWrite(b *testing.B) {
	for _, n := range fleetSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			s := benchStore(b, n)
			ctx, stop := context.WithCancel(context.Background())
			var (
				wg       sync.WaitGroup
				once     sync.Once
				ready    = make(chan struct{})
				writeErr error
			)
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Never leave the benchmark waiting if the first commit fails.
				defer once.Do(func() { close(ready) })
				for i := 0; ctx.Err() == nil; i++ {
					if err := touchAgent(ctx, s, n, i); err != nil && ctx.Err() == nil {
						writeErr = err
						return
					}
					once.Do(func() { close(ready) })
				}
			}()
			b.Cleanup(func() { stop(); wg.Wait() })

			// Start measuring once a writer is running.
			<-ready
			storagetest.Measure(b, func(i int) error {
				_, err := s.GetAgent(context.Background(), storagetest.AgentID(i%n))
				return err
			})
			stop()
			wg.Wait()
			if writeErr != nil {
				b.Fatalf("background writer: %v", writeErr)
			}
		})
	}
}

// BenchmarkListPage is the first page of an agent listing, as the API and the UI
// ask for it, over a fleet of 5 000: the store has to find the 100 agents of the
// page among all of them.
func BenchmarkListPage(b *testing.B) {
	const agents, limit = 5000, 100
	s := benchStore(b, agents)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		page, err := s.ListAgents(ctx, nil, storage.ListOptions{Limit: limit})
		if err != nil {
			b.Fatal(err)
		}
		if len(page.Items) != limit || page.NextCursor == "" {
			b.Fatalf("first page of %d agents has %d items and cursor %q, want %d and a cursor", agents, len(page.Items), page.NextCursor, limit)
		}
	}
}
