package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

// startRunner starts r and returns the channel of Start's result, once the
// listener is bound.
func startRunner(t *testing.T, r *Runner) <-chan error {
	t.Helper()
	started := make(chan error, 1)
	go func() { started <- r.Start(context.Background()) }()
	<-r.Bound()
	if r.Addr() == "" {
		t.Fatalf("Start did not listen: %v", <-started)
	}
	return started
}

// Stop lets a request in flight finish before it returns, and the request is
// answered in full: its context is not cancelled by the shutdown.
func TestStopLetsRequestsInFlightFinish(t *testing.T) {
	var (
		entered   = make(chan struct{})
		release   = make(chan struct{})
		cancelled = make(chan struct{}, 1)
	)
	r, err := New(Config{Addr: "127.0.0.1:0"}, zerolog.Nop(), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		select {
		case <-release:
			_, _ = w.Write([]byte("done"))
		case <-req.Context().Done():
			cancelled <- struct{}{}
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	started := startRunner(t, r)
	type answer struct {
		body string
		err  error
	}
	answered := make(chan answer, 1)
	go func() {
		resp, err := http.Get("http://" + r.Addr() + "/")
		if err != nil {
			answered <- answer{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		answered <- answer{string(b), err}
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- r.Stop(ctx) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned (%v) while a request was in flight", err)
	case <-cancelled:
		t.Fatal("the shutdown cancelled the context of a request in flight")
	case <-time.After(200 * time.Millisecond): // a deliberate window: Stop must not return in it
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if a := <-answered; a.err != nil || a.body != "done" {
		t.Fatalf("the request in flight: %q, %v; want its full answer", a.body, a.err)
	}
}

// OnShutdown runs as Stop begins, and a handler it tells to end, as a stream is,
// does not hold Stop until its deadline.
func TestOnShutdownEndsAStream(t *testing.T) {
	var (
		entered = make(chan struct{})
		end     = make(chan struct{})
	)
	r, err := New(Config{Addr: "127.0.0.1:0", OnShutdown: func() { close(end) }}, zerolog.Nop(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		close(entered)
		<-end
	}))
	if err != nil {
		t.Fatal(err)
	}
	started := startRunner(t, r)
	answered := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + r.Addr() + "/")
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		answered <- err
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v: the stream did not end with OnShutdown", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("the stream: %v", err)
	}
}

// A request still running when Stop's deadline passes is cut: its context is
// cancelled, and Stop reports the timeout.
func TestStopCutsARequestThatOutlastsTheDeadline(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	r, err := New(Config{Addr: "127.0.0.1:0"}, zerolog.Nop(), http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		close(entered)
		<-req.Context().Done()
		close(cancelled)
	}))
	if err != nil {
		t.Fatal(err)
	}
	started := startRunner(t, r)
	go func() {
		if resp, err := http.Get("http://" + r.Addr() + "/"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop: %v, want the deadline", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the context of the cut request was not cancelled within 5s")
	}
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
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
