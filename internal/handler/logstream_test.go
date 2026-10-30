package handler

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/encoding/protojson"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/internal/loghub"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
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

// serveLogStream serves the log stream route over a real http.Server with the
// given write timeout; zero keeps the server default of no timeout. Like the
// main listener it wraps the route in the logging and recovery middleware,
// which both wrap the ResponseWriter.
func serveLogStream(t *testing.T, open loghub.OpenIdentityFunc, writeTimeout time.Duration) *httptest.Server {
	t.Helper()
	logs := loghub.NewIdentity(open, loghub.Options{})
	t.Cleanup(logs.Close)
	api := &API{logHub: logs, logger: zerolog.Nop()}

	mux := http.NewServeMux()
	route.HandleFunc(mux, "GET /api/v1/agents/{id}/tasks/{taskID}/logs/stream", api.AgentTaskLogsStream,
		middleware.Logger(zerolog.Nop()))
	srv := httptest.NewUnstartedServer(middleware.Recovery(zerolog.Nop())(mux))
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// openLogStream connects to the log stream and returns its frames: the lines up
// to each blank line, joined by "\n". The channel closes when the stream ends.
// The connection closes at the end of the test, before the server does.
func openLogStream(t *testing.T, srv *httptest.Server) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+logStreamPath, nil)
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
		t.Fatalf("log stream answered %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("log stream Content-Type %q, want text/event-stream", ct)
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
			t.Fatal("log stream ended")
		}
		return frame
	case <-time.After(streamWait):
		t.Fatalf("no frame within %v", streamWait)
	}
	return ""
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
// beginning of the stream would fire half an interval after the chunk.
func TestLogStreamKeepaliveCountsFromLastWrite(t *testing.T) {
	const interval = time.Second
	withStreamKeepalive(t, interval)
	src := make(chan *taskv1.StreamTaskLogsResponse)
	srv := serveLogStream(t, channelSource(src), 0)

	// The handler flushes the response header right before it starts timing.
	frames := openLogStream(t, srv)
	time.Sleep(interval / 2)
	sendLog(t, src, logChunk("one"))

	var wrote time.Time
	for {
		got := nextFrame(t, frames)
		if got == keepaliveFrame {
			continue
		}
		if line := frameLine(t, got); line != "one" {
			t.Fatalf("stream wrote chunk %q, want %q", line, "one")
		}
		wrote = time.Now()
		break
	}
	if got := nextFrame(t, frames); got != keepaliveFrame {
		t.Fatalf("idle stream wrote %q, want the %q comment", got, keepaliveFrame)
	}
	if idle := time.Since(wrote); idle < interval*3/4 {
		t.Fatalf("keepalive came %v after a chunk, want a full %v of silence", idle, interval)
	}
}

// TestLogStreamNeedsWriteDeadlineControl: a writer that cannot clear the write
// deadline is refused before the stream opens, rather than streamed to until
// the server cuts it.
func TestLogStreamNeedsWriteDeadlineControl(t *testing.T) {
	var opens atomic.Int32
	logs := loghub.NewIdentity(func(context.Context, string, string, string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
		opens.Add(1)
		return make(chan *taskv1.StreamTaskLogsResponse), nil
	}, loghub.Options{})
	t.Cleanup(logs.Close)
	api := &API{logHub: logs, logger: zerolog.Nop()}

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
