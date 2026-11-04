package httpserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// A server that never got a request stops: Start makes the cancel of the base
// context before it serves, and Stop, which no connection orders after the
// serving goroutine, reads it with no data race.
func TestStopAnIdleServer(t *testing.T) {
	r, err := New(Config{Addr: "127.0.0.1:0"}, zerolog.Nop(), http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- r.Start(context.Background()) }()
	<-r.ready
	// A deliberate delay, not a wait for a condition: Serve asks for the base
	// context without a signal, and Stop must come after that to meet the race
	// that Start once had.
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// Stop cancels the context of a request in flight, which ends a handler that
// waits for it, as a stream does, so that the shutdown is graceful.
func TestStopCancelsTheContextOfRequestsInFlight(t *testing.T) {
	entered := make(chan struct{})
	r, err := New(Config{Addr: "127.0.0.1:0"}, zerolog.Nop(), http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		close(entered)
		<-req.Context().Done()
	}))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- r.Start(context.Background()) }()
	<-r.ready
	if r.ln == nil {
		t.Fatalf("Start did not listen: %v", <-started)
	}
	answered := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + r.ln.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
		answered <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not reach the handler within 5s")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v: the handler did not end with the context of its request", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("the request in flight: %v", err)
	}
}
