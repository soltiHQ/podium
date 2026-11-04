package base

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// leaderTerms is a Leadership that leads for n terms, one after the other,
// each ended by end. term is the current one, from 1; the task reads it on the
// goroutine of WhenLeader. As raft's WhenLeader does, it calls fn again when fn
// returns while the term lasts.
type leaderTerms struct {
	n    int
	term int

	mu     sync.Mutex
	cancel context.CancelFunc
}

func (*leaderTerms) AmLeader() bool        { return true }
func (*leaderTerms) CurrentLeader() string { return "" }

func (l *leaderTerms) WhenLeader(ctx context.Context, fn func(context.Context) error) error {
	for l.term = 1; l.term <= l.n; l.term++ {
		term, cancel := context.WithCancel(ctx)
		l.mu.Lock()
		l.cancel = cancel
		l.mu.Unlock()
		for term.Err() == nil {
			if err := fn(term); err != nil {
				cancel()
				return err
			}
		}
		cancel()
		if ctx.Err() != nil {
			return nil
		}
	}
	return nil
}

func (l *leaderTerms) end() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancel()
}

// lockedBuffer is a bytes.Buffer safe for the runner and the test at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns the lines logged so far whose level is one of levels.
func (b *lockedBuffer) lines(t *testing.T, levels ...string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b.buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if slices.Contains(levels, entry["level"].(string)) {
			out = append(out, entry)
		}
	}
	return out
}

// field returns the values of key in lines, in order.
func field(lines []map[string]any, key string) []any {
	out := make([]any, 0, len(lines))
	for _, line := range lines {
		out = append(out, line[key])
	}
	return out
}

// A term task runs at the start of each leadership term until it succeeds, then
// waits for the term to end, which WhenLeader needs to call it once per term. A
// failure while the replica leads logs one WARN and is tried again after a pause
// that doubles, up to its cap. A leadership loss, or the end of the term, ends
// the term's attempts with one INFO and no pause, however long the term lasts
// after it: the next term runs the task again.
func TestTermTaskRetriesWhileLeaderAndRunsAgainEachTerm(t *testing.T) {
	leadership := &leaderTerms{n: 3}
	errStore := errors.New("store unavailable")
	attempts := map[int]int{}
	logs := &lockedBuffer{}
	r := NewTermTask("seed", leadership, zerolog.New(logs), func(context.Context) error {
		attempts[leadership.term]++
		switch leadership.term {
		case 1: // four failures, then the task is done, and the term ends
			if attempts[1] <= 4 {
				return errStore
			}
			leadership.end()
			return nil
		case 2: // a leadership loss, and the term lasts a while longer
			time.AfterFunc(50*time.Millisecond, leadership.end)
			return fmt.Errorf("upsert: %w", cluster.ErrNotLeader)
		default: // the term ends while the task fails
			leadership.end()
			return errStore
		}
	})
	r.retryPause, r.maxRetryPause = time.Millisecond, 4*time.Millisecond
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	if want := map[int]int{1: 5, 2: 1, 3: 1}; !maps.Equal(attempts, want) {
		t.Fatalf("attempts per term %v, want %v", attempts, want)
	}
	warns := logs.lines(t, "warn")
	if got, want := field(warns, "retry_in"), []any{1.0, 2.0, 4.0, 4.0}; !slices.Equal(got, want) {
		t.Fatalf("WARN pauses %v ms, want %v", got, want)
	}
	if got, want := field(warns, "attempt"), []any{1.0, 2.0, 3.0, 4.0}; !slices.Equal(got, want) {
		t.Fatalf("WARN attempts %v, want %v", got, want)
	}
	var ends []map[string]any
	for _, line := range logs.lines(t, "info") {
		if line["message"] != "runner started" && line["message"] != "runner acquired leadership" {
			ends = append(ends, line)
		}
	}
	if got, want := field(ends, "message"), []any{termTaskDone, termTaskEnded, termTaskEnded}; !slices.Equal(got, want) {
		t.Fatalf("INFO at the end of each term %v, want %v", got, want)
	}
	if got, want := field(ends, "attempts"), []any{5.0, 1.0, 1.0}; !slices.Equal(got, want) {
		t.Fatalf("attempts logged at the end of each term %v, want %v", got, want)
	}
}

// Stop ends a term task that waits out a pause at once: the pause is the term's,
// not the shutdown's.
func TestTermTaskStopsDuringAPause(t *testing.T) {
	logs := &lockedBuffer{}
	r := NewTermTask("seed", &leaderTerms{n: 1}, zerolog.New(logs), func(context.Context) error { return errors.New("store unavailable") })
	r.retryPause, r.maxRetryPause = time.Hour, time.Hour
	started := make(chan error, 1)
	go func() { started <- r.Start(context.Background()) }()
	deadline := time.Now().Add(2 * time.Second)
	for len(logs.lines(t, "warn")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the task did not fail within 2s")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop during a pause: %v", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// The pause after a failure starts at a second and never exceeds a minute.
func TestTermTaskPauses(t *testing.T) {
	r := NewTermTask("seed", &leaderTerms{}, zerolog.Nop(), func(context.Context) error { return nil })
	if r.retryPause != time.Second || r.maxRetryPause != time.Minute {
		t.Fatalf("pauses from %s up to %s, want from 1s up to 1m", r.retryPause, r.maxRetryPause)
	}
}
