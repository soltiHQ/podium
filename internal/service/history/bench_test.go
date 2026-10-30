package history

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage/durable"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// benchStore opens a durable store in a fresh directory and seeds it.
func benchStore(b *testing.B, f storagetest.Fixture) *durable.Store {
	b.Helper()
	s, err := durable.Open(filepath.Join(b.TempDir(), "state"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	if err = storagetest.Seed(context.Background(), s, f); err != nil {
		b.Fatal(err)
	}
	return s
}

// BenchmarkHistoryPrune is the retention pass of an idle collector: the history
// is within every limit, so nothing is due and nothing may be written.
func BenchmarkHistoryPrune(b *testing.B) {
	for _, rows := range []int{1000, 10000} {
		b.Run(strconv.Itoa(rows), func(b *testing.B) {
			ctx := context.Background()
			svc, err := New(benchStore(b, storagetest.Fixture{HistoryRows: rows}), DefaultConfig())
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
}
