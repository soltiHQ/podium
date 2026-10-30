package event

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transportctx"
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

// allowAccess is a stream access re-check that always passes.
func allowAccess(*http.Request) error { return nil }

// serverWrite is one Write call of the stream handler, stamped on the server
// when the handler made it.
type serverWrite struct {
	at   time.Time
	data string
}

// writeLog records the Write calls of the stream handlers of a server.
type writeLog struct {
	mu     sync.Mutex
	writes []serverWrite
}

func (l *writeLog) add(p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writes = append(l.writes, serverWrite{at: time.Now(), data: string(p)})
}

// firstAfter returns the first write at or after index from whose data starts
// with prefix, and its index; -1 if there is none.
func (l *writeLog) firstAfter(from int, prefix string) (serverWrite, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := from; i < len(l.writes); i++ {
		if strings.HasPrefix(l.writes[i].data, prefix) {
			return l.writes[i], i
		}
	}
	return serverWrite{}, -1
}

// stampedWriter records the handler's writes in log. It keeps the Flusher of
// the writer it wraps and unwraps to it, so the handler still flushes and
// sets write deadlines.
type stampedWriter struct {
	http.ResponseWriter
	log *writeLog
}

func (w stampedWriter) Write(p []byte) (int, error) {
	w.log.add(p)
	return w.ResponseWriter.Write(p)
}

func (w stampedWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

func (w stampedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// smallSendBuffers shrinks the send buffer of every connection it accepts, so
// a client that stops reading blocks the server's writes after a few KiB.
type smallSendBuffers struct{ net.Listener }

func (l smallSendBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4 << 10)
	}
	return c, err
}

// sseOptions configures serveSSEWith.
type sseOptions struct {
	writeTimeout time.Duration             // zero keeps the server default of no timeout
	access       func(*http.Request) error // nil: allowAccess
	identity     *identity.Identity        // stored in each request context, as Auth does
	smallBuffers bool                      // see smallSendBuffers
}

// sseServer is a fresh hub's event stream served over a real http.Server.
type sseServer struct {
	hub    *Hub
	srv    *httptest.Server
	writes *writeLog     // the handler's writes, stamped server side
	ended  chan struct{} // closed when the first handler run returns
}

// serveSSE serves a fresh hub's event stream over a real http.Server with the
// given write timeout; zero keeps the server default of no timeout.
func serveSSE(t *testing.T, writeTimeout time.Duration) (*Hub, *httptest.Server) {
	t.Helper()
	s := serveSSEWith(t, sseOptions{writeTimeout: writeTimeout})
	return s.hub, s.srv
}

func serveSSEWith(t *testing.T, opts sseOptions) *sseServer {
	t.Helper()
	access := opts.access
	if access == nil {
		access = allowAccess
	}
	s := &sseServer{hub: newTestHub(), writes: &writeLog{}, ended: make(chan struct{})}
	t.Cleanup(s.hub.Close)
	stream := s.hub.SSEHandler(access)
	var endOnce sync.Once
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer endOnce.Do(func() { close(s.ended) })
		if opts.identity != nil {
			r = r.WithContext(transportctx.WithIdentity(r.Context(), opts.identity))
		}
		stream(stampedWriter{ResponseWriter: w, log: s.writes}, r)
	}))
	s.srv.Config.WriteTimeout = opts.writeTimeout
	if opts.smallBuffers {
		s.srv.Listener = smallSendBuffers{s.srv.Listener}
	}
	s.srv.Start()
	t.Cleanup(s.srv.Close)
	return s
}

// streamClient returns a client for one stream test: it waits streamWait at
// most for a response header, and never for the body.
func streamClient(t *testing.T, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	t.Helper()
	transport := &http.Transport{ResponseHeaderTimeout: streamWait, DialContext: dial}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

// startStream sends GET url with client and returns the response, which must
// be a 200. The connection closes at the end of the test, before the server
// does.
func startStream(t *testing.T, client *http.Client, url string) (*http.Response, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	resp, err := client.Do(req)
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
	return resp, ctx
}

// openStream connects to the stream and returns its frames: the lines up to
// each blank line, joined by "\n". The channel closes when the stream ends.
func openStream(t *testing.T, url string) <-chan string {
	t.Helper()
	resp, ctx := startStream(t, streamClient(t, nil), url)

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

// waitStreamEnd reads frames until the stream ends.
func waitStreamEnd(t *testing.T, frames <-chan string) {
	t.Helper()
	deadline := time.After(streamWait)
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("stream still open %v after access was lost", streamWait)
		}
	}
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

// The event stream tells nginx not to buffer it, like the task log stream.
func TestEventStreamDisablesProxyBuffering(t *testing.T) {
	_, srv := serveSSE(t, 0)
	resp, _ := startStream(t, streamClient(t, nil), srv.URL)
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering %q, want %q", got, "no")
	}
}

// TestEventStreamKeepaliveCountsFromLastWrite: a stream that wrote an event
// stays silent for another full interval. The tick that a keepalive started at
// the beginning of the stream would fire half an interval after the event. The
// server stamps both writes, so the check needs no margin.
func TestEventStreamKeepaliveCountsFromLastWrite(t *testing.T) {
	const interval = time.Second
	withKeepalive(t, interval)
	s := serveSSEWith(t, sseOptions{})

	frames := openStream(t, s.srv.URL)
	waitSubscribed(t, s.hub)
	time.Sleep(interval / 2)
	s.hub.Notify(RefreshSpecs)

	for {
		got := nextFrame(t, frames)
		if got == "data: "+RefreshSpecs {
			break
		}
		if got != keepaliveFrame {
			t.Fatalf("stream wrote %q, want the event or a keepalive", got)
		}
	}
	if got := nextFrame(t, frames); got != keepaliveFrame {
		t.Fatalf("idle stream wrote %q, want the %q comment", got, keepaliveFrame)
	}
	event, i := s.writes.firstAfter(0, "data: "+RefreshSpecs)
	if i < 0 {
		t.Fatal("server recorded no event write")
	}
	keepalive, j := s.writes.firstAfter(i, keepaliveFrame)
	if j < 0 {
		t.Fatal("server recorded no keepalive write after the event")
	}
	if idle := keepalive.at.Sub(event.at); idle < interval {
		t.Fatalf("keepalive written %v after the event, want a full %v of silence", idle, interval)
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

// TestEventStreamNeedsWriteDeadlineControl: a writer that cannot set a write
// deadline is refused rather than streamed to until the server cuts it.
func TestEventStreamNeedsWriteDeadlineControl(t *testing.T) {
	h := newTestHub()
	defer h.Close()
	// The deadline only turns a handler that streams anyway into a failure.
	ctx, cancel := context.WithTimeout(context.Background(), streamWait)
	defer cancel()

	// ResponseRecorder flushes but has no SetWriteDeadline.
	w := httptest.NewRecorder()
	h.SSEHandler(allowAccess)(w, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))

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

// sessionOutage fails every session read while down is set, as an unreachable
// store would.
type sessionOutage struct {
	storage.Storage
	down *atomic.Bool
}

func (s sessionOutage) GetSession(ctx context.Context, id string) (*model.Session, error) {
	if s.down.Load() {
		return nil, storage.ErrUnavailable
	}
	return s.Storage.GetSession(ctx, id)
}

// streamViewer is a user with a live session in an in-memory store, and the
// access re-check app gives the event stream: authentication only.
type streamViewer struct {
	store *inmemory.Store
	user  *model.User
	down  atomic.Bool // the session store fails while set
	id    *identity.Identity
	check func(*http.Request) error
}

func newStreamViewer(t *testing.T) *streamViewer {
	t.Helper()
	ctx := context.Background()
	v := &streamViewer{store: inmemory.New(), id: &identity.Identity{UserID: "viewer", SessionID: "viewer-session"}}
	u, err := model.NewUser("viewer", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	u.Enable()
	u.SetPermissions([]string{string(enum.AgentsGet), string(enum.SpecsGet)})
	if err = v.store.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	v.user = u
	sess, err := model.NewSession("viewer-session", "viewer", "c", enum.Password, make([]byte, 32), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = v.store.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	sessions := kit.New(sessionOutage{Storage: v.store, down: &v.down}, auth.Config{JWTSecret: strings.Repeat("k", auth.MinJWTSecretLen)}).Session
	v.check = middleware.Recheck(sessions, "", zerolog.Nop())
	return v
}

// gatedCheck hands each verdict of check to the test and holds the stream
// until the test releases it, so the test changes access between two checks.
type gatedCheck struct {
	check   func(*http.Request) error
	verdict chan error
	resume  chan struct{}
}

func newGatedCheck(check func(*http.Request) error) *gatedCheck {
	return &gatedCheck{check: check, verdict: make(chan error), resume: make(chan struct{})}
}

func (g *gatedCheck) run(r *http.Request) error {
	err := g.check(r)
	select {
	case g.verdict <- err:
	case <-r.Context().Done():
		return err
	}
	select {
	case <-g.resume:
	case <-r.Context().Done():
	}
	return err
}

// next returns the verdict of the next check, which holds the stream until
// release.
func (g *gatedCheck) next(t *testing.T) error {
	t.Helper()
	select {
	case err := <-g.verdict:
		return err
	case <-time.After(streamWait):
		t.Fatalf("no access re-check within %v", streamWait)
	}
	return nil
}

func (g *gatedCheck) release(t *testing.T) {
	t.Helper()
	select {
	case g.resume <- struct{}{}:
	case <-time.After(streamWait):
		t.Fatalf("stream did not wait for the re-check within %v", streamWait)
	}
}

// TestEventStreamEndsWhenAccessIsLost: the event stream re-checks the viewer's
// authentication every keepalive interval, also while it sends events, and the
// first check after the viewer lost it, or that cannot be made, ends the
// stream. Losing a permission is not losing authentication: the stream stays.
func TestEventStreamEndsWhenAccessIsLost(t *testing.T) {
	const keepalive = 50 * time.Millisecond
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, v *streamViewer)
		ends   bool
	}{
		{"session revoked", func(t *testing.T, v *streamViewer) {
			if err := v.store.RevokeSession(context.Background(), "viewer-session", time.Now()); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"user disabled", func(t *testing.T, v *streamViewer) {
			v.user.Disable()
			if err := v.store.UpsertUser(context.Background(), v.user); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"storage error", func(_ *testing.T, v *streamViewer) { v.down.Store(true) }, true},
		{"AgentsGet removed", func(t *testing.T, v *streamViewer) {
			v.user.SetPermissions([]string{string(enum.SpecsGet)})
			if err := v.store.UpsertUser(context.Background(), v.user); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		for _, busy := range []bool{false, true} {
			name := tc.name + "/idle"
			if busy {
				name = tc.name + "/busy"
			}
			t.Run(name, func(t *testing.T) {
				withKeepalive(t, keepalive)
				viewer := newStreamViewer(t)
				gate := newGatedCheck(viewer.check)
				s := serveSSEWith(t, sseOptions{access: gate.run, identity: viewer.id})
				frames := openStream(t, s.srv.URL)
				if busy {
					waitSubscribed(t, s.hub)
					notifyEvery(t, s.hub, RefreshAgents, keepalive/5)
				}

				if err := gate.next(t); err != nil {
					t.Fatalf("re-check before the change: %v, want access", err)
				}
				tc.change(t, viewer)
				gate.release(t)
				err := gate.next(t)
				switch {
				case tc.ends && err == nil:
					t.Fatal("the re-check after the change passed")
				case !tc.ends && err != nil:
					t.Fatalf("the re-check after the change: %v, want access", err)
				}
				gate.release(t)
				if tc.ends {
					waitStreamEnd(t, frames)
					return
				}
				if err := gate.next(t); err != nil {
					t.Fatalf("a later re-check: %v, want access", err)
				}
				gate.release(t)
				select {
				case <-s.ended:
					t.Fatal("the stream ended without losing authentication")
				default:
				}
			})
		}
	}
}

// TestEventStreamDropsStalledReader: a client that stops reading is dropped
// once a write has waited 2 × streamKeepalive, instead of holding the handler,
// its stream slot and its hub subscription until the TCP connection dies.
func TestEventStreamDropsStalledReader(t *testing.T) {
	withKeepalive(t, 50*time.Millisecond)
	s := serveSSEWith(t, sseOptions{smallBuffers: true})
	startStalledReader(t, s.srv.URL)
	waitSubscribed(t, s.hub)

	// 2 MiB of events, far more than the shrunk socket buffers hold; each
	// fits the client's channel buffer.
	event := strings.Repeat("x", 128<<10)
	for range clientBufSize {
		s.hub.Notify(event)
	}
	select {
	case <-s.ended:
	case <-time.After(streamWait):
		t.Fatalf("event stream still writing to a client that stopped reading after %v", streamWait)
	}
}

// startStalledReader requests url and reads the response header, but never
// the body. Its connection's receive buffer is small, so the server's writes
// soon block.
func startStalledReader(t *testing.T, url string) {
	t.Helper()
	dialer := &net.Dialer{}
	client := streamClient(t, func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(4 << 10)
		}
		return c, err
	})
	startStream(t, client, url)
}
