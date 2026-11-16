package sqlstore_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/database"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// The benchmark of the SQL store against the in-memory store, in a package of
// its own, as it fences the SQL store with the database leadership, which
// imports the store. The numbers and how they were taken are in the README's
// "Performance".

// listings is how many filtered listings a sub-benchmark of BenchmarkAgentSyncs
// times, once its syncs are timed.
const listings = 20

// benchBackends are the backends BenchmarkAgentSyncs compares: the SQL store on
// Postgres, fenced as in the database mode, and the in-memory store of the
// memory mode.
var benchBackends = []struct {
	name string
	open func(b *testing.B) storage.Storage
}{
	{"postgres", fencedPostgres},
	{"memory", func(*testing.B) storage.Storage { return inmemory.New() }},
}

// fencedPostgres returns a SQL store on a fresh database of its own, migrated,
// on a pool capped as the database mode's primary is by default, whose write
// transactions the database leadership fences as in the database mode: a
// replica of its own takes the lease, and holds it until b ends. It skips b
// while PODIUM_TEST_POSTGRES_URL is unset.
func fencedPostgres(b *testing.B) storage.Storage {
	b.Helper()
	databaseURL := pgtest.NewDatabase(b)
	primary := pgtest.Open(b, databaseURL)
	primary.SetMaxOpenConns(cluster.DefaultDatabaseMaxOpenConns)
	primary.SetMaxIdleConns(cluster.DefaultDatabaseMaxOpenConns)
	if err := sqlstore.Migrate(b.Context(), primary, sqlstore.Postgres{}); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	lease := pgtest.Open(b, databaseURL)
	lease.SetMaxOpenConns(1)
	lead, err := database.New(database.Options{
		DB: primary, LeaseDB: lease, ReplicaID: "bench", AdvertiseAddr: "127.0.0.1:0", Logger: zerolog.Nop(),
	})
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_ = lead.Run(ctx)
	}()
	b.Cleanup(func() {
		cancel()
		<-ran
	})
	deadline := time.Now().Add(10 * time.Second)
	for !lead.AmLeader() {
		if time.Now().After(deadline) {
			b.Fatal("the replica took no lease within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	store, err := sqlstore.Open(b.Context(), sqlstore.Options{Primary: primary, Dialect: sqlstore.Postgres{}, Fence: lead})
	if err != nil {
		b.Fatalf("open the store: %v", err)
	}
	return store
}

// BenchmarkAgentSyncs measures, on each backend, with the P2a fixture of
// storagetest at 100, 1000 and 5000 agents, the sync of an enrolled agent as
// discovery runs it: verify its token, a write transaction that writes nothing
// while the token matches, read the agent, then upsert it, a write transaction.
// Discovery serves them one write transaction at a time per replica, behind the
// store's gate, so the benchmark runs them in turn. It reports the syncs a
// second (syncs/s), the median and the 99th percentile of one
// (storagetest.Measure: p50-ms, p99-ms), and, once the syncs are timed, the
// median of listings filtered listings as the UI's agent search runs them,
// through the factory's criteria, each a search for the ID of one agent, which
// matches that agent alone, so that the listing reads every row (list-p50-ms).
func BenchmarkAgentSyncs(b *testing.B) {
	for _, backend := range benchBackends {
		for _, n := range []int{100, 1000, 5000} {
			b.Run(fmt.Sprintf("%s/%d", backend.name, n), func(b *testing.B) {
				s := backend.open(b)
				ctx := context.Background()
				if err := storagetest.Seed(ctx, s, storagetest.Fixture{Agents: n}); err != nil {
					b.Fatalf("seed: %v", err)
				}
				svc := agent.New(s, zerolog.Nop())
				storagetest.Measure(b, func(i int) error {
					// The payload is built per sync, as discovery does. An agent
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
					_, err = svc.Upsert(ctx, a)
					return err
				})
				b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "syncs/s")
				b.ReportMetric(listingLatency(b, svc, storagetest.AgentID(n/2)), "list-p50-ms")
			})
		}
	}
}

// listingLatency runs listings searches for the agent whose ID is query, each
// as the UI's agent search runs it (the agent service's listing of the
// factory's criteria, its default page, on a context marked read-only as a GET
// is), and returns their median, in milliseconds.
func listingLatency(b *testing.B, svc *agent.Service, query string) float64 {
	b.Helper()
	ctx := storage.MarkReadOnly(context.Background())
	took := make([]time.Duration, 0, listings)
	for range listings {
		start := time.Now()
		page, err := svc.List(ctx, agent.ListQuery{Criteria: storage.AgentQueryCriteria{Query: query}})
		took = append(took, time.Since(start))
		if err != nil {
			b.Fatalf("list: %v", err)
		}
		if len(page.Items) != 1 || page.Items[0].ID() != query {
			b.Fatalf("the search for %s listed %d agents, want that one alone", query, len(page.Items))
		}
	}
	slices.Sort(took)
	return float64(took[len(took)/2]) / float64(time.Millisecond)
}
