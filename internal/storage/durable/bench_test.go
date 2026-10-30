package durable

import (
	"context"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

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

// measure runs op once per iteration and reports the median and the 99th
// percentile of its latency next to the mean the framework prints: a commit
// costs a sync, and one slow sync moves the mean but not the median. Below 100
// iterations the 99th percentile is the slowest one.
func measure(b *testing.B, op func(i int) error) {
	b.Helper()
	samples := make([]time.Duration, 0, 1024)
	for i := 0; b.Loop(); i++ {
		start := time.Now()
		err := op(i)
		samples = append(samples, time.Since(start))
		if err != nil {
			b.Fatal(err)
		}
	}
	if len(samples) == 0 {
		return
	}
	slices.Sort(samples)
	quantile := func(q float64) float64 {
		rank := max(int(math.Ceil(q*float64(len(samples))))-1, 0)
		return float64(samples[rank]) / float64(time.Millisecond)
	}
	b.ReportMetric(quantile(0.50), "p50-ms")
	b.ReportMetric(quantile(0.99), "p99-ms")
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
			measure(b, func(int) error { return s.WithTx(ctx, noop) })
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
			measure(b, func(i int) error { return touchAgent(ctx, s, n, i) })
		})
	}
}

// BenchmarkHeartbeat is the heartbeat of an enrolled agent as discovery runs
// it: verify the token, which writes nothing while it matches, then upsert the
// agent.
func BenchmarkHeartbeat(b *testing.B) {
	for _, n := range fleetSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			s := benchStore(b, n)
			svc := agent.New(s, zerolog.Nop())
			ctx := context.Background()
			b.ReportAllocs()
			measure(b, func(i int) error {
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
				return svc.Upsert(ctx, a)
			})
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
			measure(b, func(i int) error {
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
