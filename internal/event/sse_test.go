package event

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const keepaliveFrame = ": keepalive"

// streamWait bounds how long a test waits for a stream. A healthy stream
// answers within milliseconds: the bound only turns a hang into a failure.
const streamWait = 5 * time.Second

// withKeepalive shortens streamKeepalive for the test.
func withKeepalive(t *testing.T, d time.Duration) {
	t.Helper()
	prev := streamKeepalive
	streamKeepalive = d
	t.Cleanup(func() { streamKeepalive = prev })
}

// serveSSE serves a fresh hub's event stream over a real http.Server with the
// given write timeout; zero keeps the server default of no timeout.
func serveSSE(t *testing.T, writeTimeout time.Duration) (*Hub, *httptest.Server) {
	t.Helper()
	h := newTestHub()
	t.Cleanup(h.Close)
	srv := httptest.NewUnstartedServer(h.SSEHandler())
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	return h, srv
}

// openStream connects to the stream and returns its frames: the lines up to
// each blank line, joined by "\n". The channel closes when the stream ends.
// The connection closes at the end of the test, before the server does.
func openStream(t *testing.T, url string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = resp.Body.Close()
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream answered %d, want 200", resp.StatusCode)
	}

	frames := make(chan string)
	go func() {
		defer close(frames)
		var lines []string
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() != "" {
				lines = append(lines, sc.Text())
				continue
			}
			select {
			case frames <- strings.Join(lines, "\n"):
				lines = nil
			case <-ctx.Done():
				return
			}
		}
	}()
	return frames
}

func nextFrame(t *testing.T, frames <-chan string) string {
	t.Helper()
	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatal("stream ended")
		}
		return frame
	case <-time.After(streamWait):
		t.Fatalf("no frame within %v", streamWait)
	}
	return ""
}

// waitSubscribed returns once the hub has a client, so a Notify reaches it.
func waitSubscribed(t *testing.T, h *Hub) {
	t.Helper()
	deadline := time.Now().Add(streamWait)
	for {
		h.mu.RLock()
		n := len(h.clients)
		h.mu.RUnlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("handler never subscribed")
		}
		time.Sleep(time.Millisecond)
	}
}

// readFor reads frames for d and passes each, with its index, to check. The
// stream ending first fails the test.
func readFor(t *testing.T, frames <-chan string, d time.Duration, check func(n int, frame string)) {
	t.Helper()
	start := time.Now()
	for n := 0; time.Since(start) < d; n++ {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatalf("stream ended after %v and %d frames, want it open for %v", time.Since(start).Round(time.Millisecond), n, d)
			}
			check(n, frame)
		case <-time.After(streamWait):
			t.Fatalf("no frame within %v", streamWait)
		}
	}
}

// notifyEvery sends event on h every interval until the end of the test.
func notifyEvery(t *testing.T, h *Hub, event string, interval time.Duration) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	go func() {
		defer close(done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				h.Notify(event)
			}
		}
	}()
}

func TestEventStreamSendsKeepalive(t *testing.T) {
	withKeepalive(t, 50*time.Millisecond)
	h, srv := serveSSE(t, 0)

	frames := openStream(t, srv.URL)
	if got := nextFrame(t, frames); got != keepaliveFrame {
		t.Fatalf("idle stream wrote %q, want the %q comment", got, keepaliveFrame)
	}

	// The keepalive is a frame of its own: an event that follows is intact.
	// The handler subscribes before its first keepalive, so this Notify
	// reaches it.
	h.Notify(RefreshAgents)
	for {
		got := nextFrame(t, frames)
		if got == keepaliveFrame {
			continue
		}
		if got != "data: "+RefreshAgents {
			t.Fatalf("event after a keepalive arrived as %q, want %q", got, "data: "+RefreshAgents)
		}
		return
	}
}

// TestEventStreamKeepaliveCountsFromLastWrite: a stream that wrote an event
// stays silent for another full interval. The tick that a keepalive started at
// the beginning of the stream would fire half an interval after the event.
func TestEventStreamKeepaliveCountsFromLastWrite(t *testing.T) {
	const interval = time.Second
	withKeepalive(t, interval)
	h, srv := serveSSE(t, 0)

	frames := openStream(t, srv.URL)
	waitSubscribed(t, h)
	time.Sleep(interval / 2)
	h.Notify(RefreshSpecs)

	var wrote time.Time
	for {
		got := nextFrame(t, frames)
		if got == "data: "+RefreshSpecs {
			wrote = time.Now()
			break
		}
		if got != keepaliveFrame {
			t.Fatalf("stream wrote %q, want the event or a keepalive", got)
		}
	}
	if got := nextFrame(t, frames); got != keepaliveFrame {
		t.Fatalf("idle stream wrote %q, want the %q comment", got, keepaliveFrame)
	}
	if idle := time.Since(wrote); idle < interval*3/4 {
		t.Fatalf("keepalive came %v after an event, want a full %v of silence", idle, interval)
	}
}

// TestEventStreamOutlivesWriteTimeout: the server cuts a handler that keeps its
// write deadline, with the connection, at the write timeout.
func TestEventStreamOutlivesWriteTimeout(t *testing.T) {
	const writeTimeout = 200 * time.Millisecond
	h, srv := serveSSE(t, writeTimeout)

	frames := openStream(t, srv.URL)
	notifyEvery(t, h, RefreshAgents, writeTimeout/2)
	readFor(t, frames, 5*writeTimeout, func(_ int, frame string) {
		if frame != "data: "+RefreshAgents {
			t.Fatalf("stream wrote %q, want an %s event", frame, RefreshAgents)
		}
	})
}

// TestEventStreamKeepaliveOutlivesWriteTimeout: a stream with nothing to say
// stays open on its keepalives alone.
func TestEventStreamKeepaliveOutlivesWriteTimeout(t *testing.T) {
	const writeTimeout = 200 * time.Millisecond
	withKeepalive(t, 50*time.Millisecond)
	_, srv := serveSSE(t, writeTimeout)

	frames := openStream(t, srv.URL)
	readFor(t, frames, 5*writeTimeout, func(_ int, frame string) {
		if frame != keepaliveFrame {
			t.Fatalf("idle stream wrote %q, want the %q comment", frame, keepaliveFrame)
		}
	})
}

// TestEventStreamNeedsWriteDeadlineControl: a writer that cannot clear the
// write deadline is refused rather than streamed to until the server cuts it.
func TestEventStreamNeedsWriteDeadlineControl(t *testing.T) {
	h := newTestHub()
	defer h.Close()
	// The deadline only turns a handler that streams anyway into a failure.
	ctx, cancel := context.WithTimeout(context.Background(), streamWait)
	defer cancel()

	// ResponseRecorder flushes but has no SetWriteDeadline.
	w := httptest.NewRecorder()
	h.SSEHandler()(w, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), ErrSSEDeadline.Error()) {
		t.Fatalf("body %q does not name %q", w.Body.String(), ErrSSEDeadline)
	}
	if ct := w.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("refused request got Content-Type %q", ct)
	}
}
