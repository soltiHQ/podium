package history

import (
	"context"
	"io"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/raft"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/durable"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// backends are the stores the benchmarks compare: the standalone durable store and
// a single-node Raft cluster.
var backends = []struct {
	name string
	open func(b *testing.B) storage.Storage
}{
	{"durable", openDurable},
	{"raft", openRaft},
}

// openDurable opens a durable store in a fresh directory.
func openDurable(b *testing.B) storage.Storage {
	b.Helper()
	s, err := durable.Open(filepath.Join(b.TempDir(), "state"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// openRaft starts a single-node Raft cluster and returns its store once the node
// leads. Its log, transport and snapshots are in memory: a prune of an idle history
// only reads, so they take no part in what it measures, and Raft's own logging stays
// out of the benchmark output.
func openRaft(b *testing.B) storage.Storage {
	b.Helper()
	cfg := hraft.DefaultConfig()
	cfg.LocalID = "bench"
	cfg.LogOutput = io.Discard
	cfg.HeartbeatTimeout = 50 * time.Millisecond
	cfg.ElectionTimeout = 50 * time.Millisecond
	cfg.LeaderLeaseTimeout = 50 * time.Millisecond
	inner := inmemory.New()
	logs := hraft.NewInmemStore()
	addr, transport := hraft.NewInmemTransport("")
	r, err := hraft.NewRaft(cfg, raft.NewFSM(inner, nil), logs, logs, hraft.NewInmemSnapshotStore(), transport)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = r.Shutdown().Error() })
	servers := []hraft.Server{{ID: cfg.LocalID, Address: addr}}
	if err = r.BootstrapCluster(hraft.Configuration{Servers: servers}).Error(); err != nil {
		b.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); r.State() != hraft.Leader; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			b.Fatal("the Raft node did not lead within two seconds")
		}
	}
	return raft.NewStore(inner, r)
}

// benchStore opens a store with open and seeds it.
func benchStore(b *testing.B, open func(*testing.B) storage.Storage, f storagetest.Fixture) storage.Storage {
	b.Helper()
	s := open(b)
	if err := storagetest.Seed(context.Background(), s, f); err != nil {
		b.Fatal(err)
	}
	return s
}

// BenchmarkHistoryPrune is the retention pass of an idle collector, on each backend:
// the history is within every limit, so nothing is due and nothing may be written.
func BenchmarkHistoryPrune(b *testing.B) {
	for _, rows := range []int{1000, 10000} {
		b.Run(strconv.Itoa(rows), func(b *testing.B) {
			for _, backend := range backends {
				b.Run(backend.name, func(b *testing.B) {
					ctx := context.Background()
					svc, err := New(benchStore(b, backend.open, storagetest.Fixture{HistoryRows: rows}), DefaultConfig())
					if err != nil {
						b.Fatal(err)
					}
					// An hour after the last row was captured: all of them are well inside
					// the retention window, whatever the wall clock says.
					svc.now = func() time.Time { return storagetest.Epoch.Add(time.Duration(rows)*time.Second + time.Hour) }
					b.ReportAllocs()
					for b.Loop() {
						removed, err := svc.Prune(ctx)
						if err != nil {
							b.Fatal(err)
						}
						if removed != 0 {
							b.Fatalf("Prune removed %d rows of a history within every limit", removed)
						}
					}
				})
			}
		})
	}
}
