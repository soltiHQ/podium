package handler

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/encoding/protojson"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/loghub"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

const (
	logStreamPath  = "/api/v1/agents/agent-1/tasks/task-1/logs/stream"
	keepaliveFrame = ": keepalive"
)

// streamWait bounds how long a test waits for a stream. A healthy stream
// answers within milliseconds: the bound only turns a hang into a failure.
const streamWait = 5 * time.Second

// withStreamKeepalive shortens streamKeepalive for the test.
func withStreamKeepalive(t *testing.T, d time.Duration) {
	t.Helper()
	prev := streamKeepalive
	streamKeepalive = d
	t.Cleanup(func() { streamKeepalive = prev })
}

// allowAccess is a stream access re-check that always passes.
func allowAccess(*http.Request) error { return nil }

func logChunk(line string) *taskv1.StreamTaskLogsResponse {
	return &taskv1.StreamTaskLogsResponse{
		Kind: &taskv1.StreamTaskLogsResponse_Chunk{Chunk: &taskv1.OutputChunk{Line: []byte(line)}},
	}
}

// channelSource opens every log stream on src.
func channelSource(src <-chan *taskv1.StreamTaskLogsResponse) loghub.OpenIdentityFunc {
	return func(context.Context, string, string, string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
		return src, nil
	}
}

// tickingSource emits the chunks "line 0", "line 1", ... every interval until
// the last viewer leaves.
func tickingSource(interval time.Duration) loghub.OpenIdentityFunc {
	return func(ctx context.Context, _, _, _ string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
		out := make(chan *taskv1.StreamTaskLogsResponse)
		go func() {
			defer close(out)
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for i := 0; ; i++ {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				select {
				case out <- logChunk(fmt.Sprintf("line %d", i)):
				case <-ctx.Done():
					return
				}
			}
		}()
		return out, nil
	}
}

// sendLog hands ev to the log hub through its source channel.
func sendLog(t *testing.T, src chan<- *taskv1.StreamTaskLogsResponse, ev *taskv1.StreamTaskLogsResponse) {
	t.Helper()
	select {
	case src <- ev:
	case <-time.After(streamWait):
		t.Fatalf("log hub did not take the event within %v", streamWait)
	}
}

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

// logServerOptions configures serveLogStreamWith.
type logServerOptions struct {
	writeTimeout time.Duration             // zero keeps the server default of no timeout
	access       func(*http.Request) error // nil: allowAccess
	identity     *identity.Identity        // stored in each request context, as Auth does
	smallBuffers bool                      // see smallSendBuffers
}

// logServer is the log stream route served over a real http.Server.
type logServer struct {
	srv    *httptest.Server
	writes *writeLog     // the handler's writes, stamped server side
	ended  chan struct{} // closed when the first handler run returns
}

// serveLogStream serves the log stream route over a real http.Server with the
// given write timeout; zero keeps the server default of no timeout.
func serveLogStream(t *testing.T, open loghub.OpenIdentityFunc, writeTimeout time.Duration) *httptest.Server {
	t.Helper()
	return serveLogStreamWith(t, open, logServerOptions{writeTimeout: writeTimeout}).srv
}

// serveLogStreamWith serves the log stream route as opts says. Like the main
// listener it wraps the route in the logging and recovery middleware, which
// both wrap the ResponseWriter.
func serveLogStreamWith(t *testing.T, open loghub.OpenIdentityFunc, opts logServerOptions) *logServer {
	t.Helper()
	logs := loghub.NewIdentity(open, loghub.Options{})
	t.Cleanup(logs.Close)
	access := opts.access
	if access == nil {
		access = allowAccess
	}
	api := &API{logHub: logs, logAccess: access, logger: zerolog.Nop()}

	ls := &logServer{writes: &writeLog{}, ended: make(chan struct{})}
	var endOnce sync.Once
	handler := func(w http.ResponseWriter, r *http.Request) {
		defer endOnce.Do(func() { close(ls.ended) })
		if opts.identity != nil {
			r = r.WithContext(transportctx.WithIdentity(r.Context(), opts.identity))
		}
		api.AgentTaskLogsStream(stampedWriter{ResponseWriter: w, log: ls.writes}, r)
	}
	mux := http.NewServeMux()
	route.HandleFunc(mux, "GET /api/v1/agents/{id}/tasks/{taskID}/logs/stream", handler,
		middleware.Logger(zerolog.Nop()))
	ls.srv = httptest.NewUnstartedServer(middleware.Recovery(zerolog.Nop())(mux))
	ls.srv.Config.WriteTimeout = opts.writeTimeout
	if opts.smallBuffers {
		ls.srv.Listener = smallSendBuffers{ls.srv.Listener}
	}
	ls.srv.Start()
	t.Cleanup(ls.srv.Close)
	return ls
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
// be a 200 event stream. The connection closes at the end of the test, before
// the server does.
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
		t.Fatalf("log stream answered %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("log stream Content-Type %q, want text/event-stream", ct)
	}
	return resp, ctx
}

// openLogStream connects to the log stream and returns its frames: the lines up
// to each blank line, joined by "\n". The channel closes when the stream ends.
func openLogStream(t *testing.T, srv *httptest.Server) <-chan string {
	t.Helper()
	resp, ctx := startStream(t, streamClient(t, nil), srv.URL+logStreamPath)

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
			t.Fatal("log stream ended")
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

// frameLine returns the log line of a chunk frame.
func frameLine(t *testing.T, frame string) string {
	t.Helper()
	payload, ok := strings.CutPrefix(frame, "data: ")
	if !ok {
		t.Fatalf("frame %q is not a data frame", frame)
	}
	var ev taskv1.StreamTaskLogsResponse
	if err := protojson.Unmarshal([]byte(payload), &ev); err != nil {
		t.Fatalf("frame %q: %v", frame, err)
	}
	return string(ev.GetChunk().GetLine())
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

// TestLogStreamOutlivesWriteTimeout: the server cuts a handler that keeps its
// write deadline, with the connection, at the write timeout.
func TestLogStreamOutlivesWriteTimeout(t *testing.T) {
	const writeTimeout = 200 * time.Millisecond
	srv := serveLogStream(t, tickingSource(100*time.Millisecond), writeTimeout)

	frames := openLogStream(t, srv)
	readFor(t, frames, 5*writeTimeout, func(n int, frame string) {
		if got, want := frameLine(t, frame), fmt.Sprintf("line %d", n); got != want {
			t.Fatalf("chunk %d is %q, want %q", n, got, want)
		}
	})
}

// TestLogStreamKeepaliveOutlivesWriteTimeout: a viewer left open on a task that
// logs nothing stays connected on the keepalives alone.
func TestLogStreamKeepaliveOutlivesWriteTimeout(t *testing.T) {
	const writeTimeout = 200 * time.Millisecond
	withStreamKeepalive(t, 50*time.Millisecond)
	srv := serveLogStream(t, channelSource(make(chan *taskv1.StreamTaskLogsResponse)), writeTimeout)

	frames := openLogStream(t, srv)
	readFor(t, frames, 5*writeTimeout, func(_ int, frame string) {
		if frame != keepaliveFrame {
			t.Fatalf("idle stream wrote %q, want the %q comment", frame, keepaliveFrame)
		}
	})
}

func TestLogStreamSendsKeepalive(t *testing.T) {
	withStreamKeepalive(t, 50*time.Millisecond)
	src := make(chan *taskv1.StreamTaskLogsResponse)
	srv := serveLogStream(t, channelSource(src), 0)

	frames := openLogStream(t, srv)
	if got := nextFrame(t, frames); got != keepaliveFrame {
		t.Fatalf("idle stream wrote %q, want the %q comment", got, keepaliveFrame)
	}

	// The keepalive is a frame of its own: a chunk that follows is intact.
	sendLog(t, src, logChunk("after keepalive"))
	for {
		got := nextFrame(t, frames)
		if got == keepaliveFrame {
			continue
		}
		if line := frameLine(t, got); line != "after keepalive" {
			t.Fatalf("chunk after a keepalive arrived as %q", line)
		}
		return
	}
}

// TestLogStreamKeepaliveCountsFromLastWrite: a stream that wrote a chunk stays
// silent for another full interval. The tick that a keepalive started at the
// beginning of the stream would fire half an interval after the chunk. The
// server stamps both writes, so the check needs no margin.
func TestLogStreamKeepaliveCountsFromLastWrite(t *testing.T) {
	const interval = time.Second
	withStreamKeepalive(t, interval)
	src := make(chan *taskv1.StreamTaskLogsResponse)
	ls := serveLogStreamWith(t, channelSource(src), logServerOptions{})

	// The handler flushes the response header right before it starts timing.
	frames := openLogStream(t, ls.srv)
	time.Sleep(interval / 2)
	sendLog(t, src, logChunk("one"))

	for {
		got := nextFrame(t, frames)
		if got == keepaliveFrame {
			continue
		}
		if line := frameLine(t, got); line != "one" {
			t.Fatalf("stream wrote chunk %q, want %q", line, "one")
		}
		break
	}
	if got := nextFrame(t, frames); got != keepaliveFrame {
		t.Fatalf("idle stream wrote %q, want the %q comment", got, keepaliveFrame)
	}
	chunk, i := ls.writes.firstAfter(0, "data: ")
	if i < 0 {
		t.Fatal("server recorded no chunk write")
	}
	keepalive, j := ls.writes.firstAfter(i, keepaliveFrame)
	if j < 0 {
		t.Fatal("server recorded no keepalive write after the chunk")
	}
	if idle := keepalive.at.Sub(chunk.at); idle < interval {
		t.Fatalf("keepalive written %v after the chunk, want a full %v of silence", idle, interval)
	}
}

// TestLogStreamNeedsWriteDeadlineControl: a writer that cannot set a write
// deadline is refused before the stream opens, rather than streamed to until
// the server cuts it.
func TestLogStreamNeedsWriteDeadlineControl(t *testing.T) {
	var opens atomic.Int32
	logs := loghub.NewIdentity(func(context.Context, string, string, string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
		opens.Add(1)
		return make(chan *taskv1.StreamTaskLogsResponse), nil
	}, loghub.Options{})
	t.Cleanup(logs.Close)
	api := &API{logHub: logs, logAccess: allowAccess, logger: zerolog.Nop()}

	// The deadline only turns a handler that streams anyway into a failure.
	ctx, cancel := context.WithTimeout(context.Background(), streamWait)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, logStreamPath, nil).WithContext(ctx)
	r.SetPathValue("id", "agent-1")
	r.SetPathValue("taskID", "task-1")

	// ResponseRecorder flushes but has no SetWriteDeadline.
	w := httptest.NewRecorder()
	api.AgentTaskLogsStream(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("refused request got Content-Type %q", ct)
	}
	if n := opens.Load(); n != 0 {
		t.Fatalf("log source opened %d times for a request that was refused", n)
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

// streamViewer is a user who may watch task logs, with a live session in an
// in-memory store, and the access re-check app gives the log stream.
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
	v.check = middleware.Recheck(sessions, enum.AgentsGet, zerolog.Nop())
	return v
}

// Ways a viewer loses access to task logs while a stream is open.
var lostLogAccess = []struct {
	name   string
	change func(t *testing.T, v *streamViewer)
}{
	{"session revoked", func(t *testing.T, v *streamViewer) {
		if err := v.store.RevokeSession(context.Background(), "viewer-session", time.Now()); err != nil {
			t.Fatal(err)
		}
	}},
	{"user disabled", func(t *testing.T, v *streamViewer) {
		v.user.Disable()
		if err := v.store.UpsertUser(context.Background(), v.user); err != nil {
			t.Fatal(err)
		}
	}},
	{"AgentsGet removed", func(t *testing.T, v *streamViewer) {
		v.user.SetPermissions([]string{string(enum.SpecsGet)})
		if err := v.store.UpsertUser(context.Background(), v.user); err != nil {
			t.Fatal(err)
		}
	}},
	{"storage error", func(_ *testing.T, v *streamViewer) { v.down.Store(true) }},
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

// TestLogStreamEndsWhenAccessIsLost: the log stream re-checks the viewer's
// access every keepalive interval, also while it streams output, and the first
// check after the viewer lost access, or that cannot be made, ends it.
// EventSource then reconnects through authentication, which refuses it.
func TestLogStreamEndsWhenAccessIsLost(t *testing.T) {
	const keepalive = 50 * time.Millisecond
	for _, tc := range lostLogAccess {
		for _, traffic := range []struct {
			name string
			open loghub.OpenIdentityFunc
		}{
			{"idle", channelSource(make(chan *taskv1.StreamTaskLogsResponse))},
			{"busy", tickingSource(keepalive / 5)},
		} {
			t.Run(tc.name+"/"+traffic.name, func(t *testing.T) {
				withStreamKeepalive(t, keepalive)
				viewer := newStreamViewer(t)
				gate := newGatedCheck(viewer.check)
				ls := serveLogStreamWith(t, traffic.open, logServerOptions{access: gate.run, identity: viewer.id})
				frames := openLogStream(t, ls.srv)

				if err := gate.next(t); err != nil {
					t.Fatalf("re-check before the change: %v, want access", err)
				}
				tc.change(t, viewer)
				gate.release(t)
				if err := gate.next(t); err == nil {
					t.Fatal("the re-check after the change passed")
				}
				gate.release(t)
				waitStreamEnd(t, frames)
			})
		}
	}
}

// TestLogStreamDropsStalledReader: a client that stops reading is dropped once
// a write has waited 2 × streamKeepalive, instead of holding the handler, its
// stream slot and the agent connection until the TCP connection dies.
func TestLogStreamDropsStalledReader(t *testing.T) {
	withStreamKeepalive(t, 50*time.Millisecond)
	const chunks = 64
	src := make(chan *taskv1.StreamTaskLogsResponse, chunks)
	ls := serveLogStreamWith(t, channelSource(src), logServerOptions{smallBuffers: true})
	startStalledReader(t, ls.srv.URL+logStreamPath)

	// 2 MiB of output, far more than the shrunk socket buffers hold.
	line := strings.Repeat("x", 32<<10)
	for range chunks {
		src <- logChunk(line)
	}
	select {
	case <-ls.ended:
	case <-time.After(streamWait):
		t.Fatalf("log stream still writing to a client that stopped reading after %v", streamWait)
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
