package history

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestCollectorParentCancellationStopsSource(t *testing.T) {
	entered := make(chan struct{})
	sourceCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		<-req.Context().Done()
		close(sourceCanceled)
	}))
	defer server.Close()
	r, s := newRunner(t, Config{CycleTimeout: time.Second})
	seedAgent(t, s, "agent", server.URL, time.Now())
	seedRollout(t, s, "agent", "spec")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() { r.tick(ctx); close(finished) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("source never entered")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("leader cancellation did not drain collector")
	}
	select {
	case <-sourceCanceled:
	case <-time.After(time.Second):
		t.Fatal("source HTTP request remained active")
	}
	rows, err := storage.AllExecutions(context.Background(), s, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("canceled collection published rows=%d err=%v", len(rows), err)
	}
}
