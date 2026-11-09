package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
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

// syncBuffer is a bytes.Buffer that the serving goroutine and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// What net/http reports on its own ErrorLog, here the panic of a handler, is a
// WARN line of the runner's logger with the server's name, not a line of Go's
// text logger on stderr.
func TestHTTPServerErrorsGoToZerolog(t *testing.T) {
	logs := &syncBuffer{}
	r, err := New(Config{Name: "ui", Addr: "127.0.0.1:0"}, zerolog.New(logs), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler failure")
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
	// The panic closes the connection, so the client sees an error.
	if resp, err := http.Get("http://" + r.ln.Addr().String() + "/"); err == nil {
		_ = resp.Body.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}

	var found bool
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("a log line that is no JSON: %q", raw)
		}
		if msg, _ := line["message"].(string); strings.Contains(msg, "handler failure") {
			found = true
			if line["level"] != "warn" || line["runner"] != "ui" {
				t.Fatalf("line %v, want level warn and runner ui", line)
			}
		}
	}
	if !found {
		t.Fatalf("no line carries the panic of the handler:\n%s", logs.String())
	}
}
