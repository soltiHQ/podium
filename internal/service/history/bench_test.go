package history

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/rs/zerolog"
	bolt "go.etcd.io/bbolt"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/raft"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// backends are the stores the benchmarks compare: the in-memory store and a
// single-node Raft cluster.
var backends = []struct {
	name string
	open func(b *testing.B) storage.Storage
}{
	{"memory", openMemory},
	{"raft", openRaft},
}

// openMemory opens an in-memory store.
func openMemory(b *testing.B) storage.Storage {
	b.Helper()
	return inmemory.New()
}

// openRaft starts a single-node Raft cluster and returns its store once the node
// leads. Its log is a BoltDB file, as in production, so a commit pays the fsync of
// the log. Its transport and snapshots are in memory, and Raft's own logging is
// discarded, which keeps it out of the benchmark output.
func openRaft(b *testing.B) storage.Storage {
	b.Helper()
	cfg := hraft.DefaultConfig()
	cfg.LocalID = "bench"
	cfg.LogOutput = io.Discard
	cfg.HeartbeatTimeout = 50 * time.Millisecond
	cfg.ElectionTimeout = 50 * time.Millisecond
	cfg.LeaderLeaseTimeout = 50 * time.Millisecond
	logs, err := boltdb.New(boltdb.Options{
		Path:        filepath.Join(b.TempDir(), "raft-log.bolt"),
		BoltOptions: &bolt.Options{Timeout: 250 * time.Millisecond},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = logs.Close() })
	inner := inmemory.New()
	addr, transport := hraft.NewInmemTransport("")
	r, err := hraft.NewRaft(cfg, raft.NewFSM(inner, nil, zerolog.Nop()), logs, logs, hraft.NewInmemSnapshotStore(), transport)
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

// dueRows is how many rows each due retention pass removes.
const dueRows = 100

// BenchmarkHistoryPrune is the retention pass of the collector, on each backend:
//
//   - idle: the history is within every limit, so nothing is due and nothing may
//     be read or written. Prune answers from its counters, without a scan or a
//     transaction.
//   - due: before each pass, with the timer stopped, dueRows rows join the history
//     and the clock moves past their retention. Each pass removes them in a
//     transaction that scans the whole history.
func BenchmarkHistoryPrune(b *testing.B) {
	for _, rows := range []int{1000, 10000} {
		b.Run(strconv.Itoa(rows), func(b *testing.B) {
			for _, backend := range backends {
				b.Run(backend.name, func(b *testing.B) {
					b.Run("idle", func(b *testing.B) { benchPrune(b, backend.open, rows, 0) })
					b.Run("due", func(b *testing.B) { benchPrune(b, backend.open, rows, dueRows) })
				})
			}
		})
	}
}

// benchPrune times Prune over a history of rows rows within every limit, to which
// due rows are added before each pass. Each pass must remove them.
//
// The limits are pinned, so that the age of the due rows is all that makes them
// due: the history sits exactly at its row limit, and the due rows add to it only
// until the pass.
func benchPrune(b *testing.B, open func(*testing.B) storage.Storage, rows, due int) {
	ctx := context.Background()
	store := benchStore(b, open, storagetest.Fixture{HistoryRows: rows})
	cfg := Config{Retention: 7 * 24 * time.Hour, MaxRecords: rows + due, MaxBytes: 1 << 40}
	svc, err := New(store, cfg)
	if err != nil {
		b.Fatal(err)
	}
	// An hour after the last row was captured: all of them are well inside the
	// retention window, whatever the wall clock says.
	now := storagetest.Epoch.Add(time.Duration(rows)*time.Second + time.Hour)
	svc.now = func() time.Time { return now }
	// The first decision reads the whole history to count it. A collector does that
	// once each time it takes leadership, so it stays out of the timed passes.
	if _, err := svc.Prune(ctx); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	pass := 0
	for b.Loop() {
		if due > 0 {
			b.StopTimer()
			if err := addDue(ctx, svc, now.Add(-cfg.Retention), pass, due); err != nil {
				b.Fatal(err)
			}
			// The clock moves past the retention of the rows just added.
			now = now.Add(time.Second)
			pass++
			b.StartTimer()
		}
		removed, err := svc.Prune(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if removed != due {
			b.Fatalf("Prune removed %d rows, want the %d due", removed, due)
		}
	}
}

// addDue captures n rows through the service, so that its counters know of them,
// a millisecond apart from just inside retention, which opens at from: a clock
// that moves on by a second leaves them all past it. pass gives each call rows of
// new identities.
func addDue(ctx context.Context, svc *Service, from time.Time, pass, n int) error {
	records := make([]*model.ExecutionRecord, n)
	for i := range n {
		code := int32(0)
		e, err := model.NewExecutionRecord(model.ExecutionRecordParams{
			AgentID:    storagetest.AgentID(i),
			AgentName:  "edge",
			TaskName:   "job-00",
			TaskUID:    fmt.Sprintf("due-%d-%d", pass, i),
			Generation: 1,
			Attempt:    1,
			Status:     "succeeded",
			ExitCode:   &code,
			CapturedAt: from.Add(time.Duration(i+1) * time.Millisecond),
		})
		if err != nil {
			return err
		}
		records[i] = e
	}
	_, err := svc.commit(ctx, records, CaptureResult{})
	return err
}
