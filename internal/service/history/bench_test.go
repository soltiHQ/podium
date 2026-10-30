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
	bolt "go.etcd.io/bbolt"

	"github.com/soltiHQ/control-plane/domain/model"
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

// dueRows is how many rows each due retention pass removes.
const dueRows = 100

// BenchmarkHistoryPrune is the retention pass of the collector, on each backend:
//
//   - idle: the history is within every limit, so nothing is due and nothing may
//     be written. Prune returns before it opens a transaction.
//   - due: before each pass, with the timer stopped, dueRows rows past retention
//     join the history. Each pass removes them in a transaction that scans the
//     whole history.
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
// due rows past retention are added before each pass. Each pass must remove them.
func benchPrune(b *testing.B, open func(*testing.B) storage.Storage, rows, due int) {
	ctx := context.Background()
	store := benchStore(b, open, storagetest.Fixture{HistoryRows: rows})
	cfg := DefaultConfig()
	svc, err := New(store, cfg)
	if err != nil {
		b.Fatal(err)
	}
	// An hour after the last row was captured: all of them are well inside the
	// retention window, whatever the wall clock says.
	now := storagetest.Epoch.Add(time.Duration(rows)*time.Second + time.Hour)
	svc.now = func() time.Time { return now }
	b.ReportAllocs()
	pass := 0
	for b.Loop() {
		if due > 0 {
			b.StopTimer()
			if err := addExpired(ctx, store, now.Add(-cfg.Retention), pass, due); err != nil {
				b.Fatal(err)
			}
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

// addExpired writes n rows captured before expiredBefore, so that the next Prune
// removes them. pass gives each call rows of new identities.
func addExpired(ctx context.Context, s storage.Storage, expiredBefore time.Time, pass, n int) error {
	return s.WithTx(ctx, func(tx storage.Storage) error {
		for i := range n {
			code := int32(0)
			e, err := model.NewExecutionRecord(model.ExecutionRecordParams{
				AgentID:    storagetest.AgentID(i),
				AgentName:  "edge",
				TaskName:   "job-00",
				TaskUID:    fmt.Sprintf("expired-%d-%d", pass, i),
				Generation: 1,
				Attempt:    1,
				Status:     "succeeded",
				ExitCode:   &code,
				CapturedAt: expiredBefore.Add(-time.Duration(n-i) * time.Second),
			})
			if err != nil {
				return err
			}
			if err = tx.UpsertExecution(ctx, e); err != nil {
				return err
			}
		}
		return nil
	})
}
