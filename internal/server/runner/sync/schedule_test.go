package sync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/durable"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

// deadlineProxy records the deadline each apply gets and when it returns.
type deadlineProxy struct {
	fakeProxy
	deadlines []time.Time
	returned  []time.Time
}

func (p *deadlineProxy) ApplyTask(ctx context.Context, sub proxy.TaskSubmission) (string, error) {
	deadline, _ := ctx.Deadline()
	p.deadlines = append(p.deadlines, deadline)
	time.Sleep(time.Millisecond) // the clock moves on before the next rollout starts
	p.returned = append(p.returned, time.Now())
	return p.fakeProxy.ApplyTask(ctx, sub)
}

// gatedProxy is an agent that accepts calls and never answers: each call blocks
// until the test opens gate or the call's context ends.
type gatedProxy struct {
	fakeProxy
	gate  chan struct{}
	calls atomic.Int32
}

func (g *gatedProxy) wait(ctx context.Context) error {
	g.calls.Add(1)
	select {
	case <-g.gate:
		return errors.New("gate opened")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gatedProxy) ApplyTask(ctx context.Context, _ proxy.TaskSubmission) (string, error) {
	return "", g.wait(ctx)
}

func (g *gatedProxy) DeleteTask(ctx context.Context, _ string) error { return g.wait(ctx) }

func (g *gatedProxy) GetTask(ctx context.Context, _ string) (*proxyv1.GetTaskResponse, error) {
	return nil, g.wait(ctx)
}

// agentErr mirrors proxy.AgentError: the boundary Kind over the op sentinel and
// the wire error, both visible to errors.Is.
type agentErr struct {
	kind errkind.Kind
	err  error
}

func (e agentErr) Error() string           { return e.err.Error() }
func (e agentErr) Unwrap() error           { return e.err }
func (e agentErr) ErrorKind() errkind.Kind { return e.kind }

// httpAnswer is what the HTTP proxy returns for a non-2xx agent response.
func httpAnswer(op error, code int, kind errkind.Kind) error {
	return agentErr{kind: kind, err: fmt.Errorf("%w: %w: %d", op, proxy.ErrUnexpectedStatus, code)}
}

// seedRollouts gives agentID n rollouts with one intent, each for its own spec.
func seedRollouts(t *testing.T, s storage.Storage, agentID string, n int, intent enum.RolloutIntent) []*model.Rollout {
	t.Helper()
	out := make([]*model.Rollout, 0, n)
	for i := range n {
		specID := fmt.Sprintf("%s-sp%d", agentID, i)
		seedSpecAndAgent(t, s, specID, agentID)
		ro, err := model.NewRollout(specID, agentID, 1)
		if err != nil {
			t.Fatal(err)
		}
		switch intent {
		case enum.RolloutIntentUninstall:
			ro.SetActualTaskID("task-" + specID)
			ro.SetIntent(enum.RolloutIntentUninstall)
		case enum.RolloutIntentNoop:
			ro.SetActualTaskID("task-" + specID)
			ro.MarkSynced(1)
		}
		if err := s.UpsertRollout(context.Background(), ro); err != nil {
			t.Fatal(err)
		}
		out = append(out, ro)
	}
	return out
}

// A push that used up its deadline is still recorded: the write has its own budget.
func TestMarkFailedRecordsAfterPushTimeout(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(t *testing.T) storage.Storage
	}{
		{"inmemory", func(*testing.T) storage.Storage { return inmemory.New() }},
		{"durable", func(t *testing.T) storage.Storage {
			s, err := durable.Open(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		}},
	} {
		t.Run(backend.name, func(t *testing.T) {
			r, _, hub := newFakeRunner(t, &fakeProxy{})
			r.store = backend.open(t)
			ro := seedRollouts(t, r.store, "agent-a", 1, enum.RolloutIntentInstall)[0]
			ro.RestoreState(enum.SyncStatusFailed, enum.RolloutIntentInstall, 0, 2, "offline", time.Now().Add(-time.Minute), time.Time{})
			if err := r.store.UpsertRollout(context.Background(), ro); err != nil {
				t.Fatal(err)
			}
			attempted, err := r.store.GetRollout(context.Background(), ro.ID())
			if err != nil {
				t.Fatal(err)
			}
			pushCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()

			r.markFailed(pushCtx, attempted, "apply task: context deadline exceeded")

			after, err := r.store.GetRollout(context.Background(), ro.ID())
			if err != nil {
				t.Fatal(err)
			}
			if after.Status() != enum.SyncStatusFailed || after.Attempts() != 3 || after.Error() != "apply task: context deadline exceeded" {
				t.Fatalf("failure not recorded: status=%s attempts=%d error=%q", after.Status(), after.Attempts(), after.Error())
			}
			events := hub.RecentEvents(event.RingSize)
			if len(events) != 1 || events[0].Kind != event.SyncFailed || events[0].Payload.ID != ro.SpecID() || events[0].Payload.Name != "n" {
				t.Fatalf("sync_failed event not recorded: %+v", events)
			}
		})
	}
}

// An agent call that returns as its push deadline passes still gets its outcome written.
func TestStateWritesOutlivePushTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		intent enum.RolloutIntent
		done   func(after *model.Rollout, err error) bool
	}{
		{"apply completes", enum.RolloutIntentInstall, func(after *model.Rollout, err error) bool {
			return err == nil && after.Status() == enum.SyncStatusSynced && after.ActualTaskID() == "task-new"
		}},
		{"delete completes", enum.RolloutIntentUninstall, func(_ *model.Rollout, err error) bool {
			return errors.Is(err, storage.ErrNotFound)
		}},
		{"observation requeues a lost task", enum.RolloutIntentNoop, func(after *model.Rollout, err error) bool {
			return err == nil && after.Intent() == enum.RolloutIntentInstall && after.Status() == enum.SyncStatusPending && after.ActualTaskID() == ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pushCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			atDeadline := func() { <-pushCtx.Done() }
			fp := &fakeProxy{applyResp: []string{"task-new"}, getErr: agentNotFound{}, onApply: atDeadline, onDelete: atDeadline, onGet: atDeadline}
			r, s, _ := newFakeRunner(t, fp)
			ro := seedRollouts(t, s, "agent-a", 1, tc.intent)[0]

			r.reconcile(pushCtx, ro.ID())

			after, err := s.GetRollout(context.Background(), ro.ID())
			if tc.done(after, err) {
				return
			}
			if err != nil {
				t.Fatalf("outcome not written after the push deadline: %v", err)
			}
			t.Fatalf("outcome not written after the push deadline: status=%s intent=%s task=%q", after.Status(), after.Intent(), after.ActualTaskID())
		})
	}
}

// An agent that accepts calls and never answers costs one push timeout per tick:
// the other agents' rollouts all complete in that tick, and the hung agent gets
// one attempt, recorded as a failure.
func TestHungAgentDoesNotStallOthers(t *testing.T) {
	hung := &gatedProxy{gate: make(chan struct{})}
	t.Cleanup(func() { close(hung.gate) })
	r, s, _ := newFakeRunner(t, &fakeProxy{})
	r.cfg.MaxConcurrency = 2
	r.cfg.PushTimeout = 200 * time.Millisecond
	r.pool = &fakePool{agents: map[string]proxy.AgentProxy{
		"http://agent-a": hung,
		"http://agent-b": &fakeProxy{applyResp: []string{"b-0", "b-1"}},
		"http://agent-c": &fakeProxy{applyResp: []string{"c-0", "c-1"}},
	}}
	healthy := append(seedRollouts(t, s, "agent-b", 2, enum.RolloutIntentInstall), seedRollouts(t, s, "agent-c", 2, enum.RolloutIntentInstall)...)
	// Seeded last, so listed first: the hung agent takes a worker slot from the start.
	stuck := seedRollouts(t, s, "agent-a", 3, enum.RolloutIntentInstall)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.tick(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("tick did not finish: the hung agent stalled it")
	}

	for _, ro := range healthy {
		after, err := s.GetRollout(context.Background(), ro.ID())
		if err != nil || after.Status() != enum.SyncStatusSynced || after.ActualTaskID() == "" {
			t.Fatalf("rollout %s of a healthy agent not completed in the tick (err=%v)", ro.ID(), err)
		}
	}
	if calls := hung.calls.Load(); calls > 1 {
		t.Fatalf("hung agent got %d calls in one tick, want at most 1", calls)
	}
	failed := 0
	for _, ro := range stuck {
		after, err := s.GetRollout(context.Background(), ro.ID())
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case after.Status() == enum.SyncStatusFailed && after.Attempts() == 1:
			failed++
		case !after.UpdatedAt().Equal(ro.UpdatedAt()):
			t.Fatalf("skipped rollout %s was changed", ro.ID())
		}
	}
	if failed != 1 {
		t.Fatalf("hung agent's attempt recorded %d failures, want 1", failed)
	}
}

// A call the agent never answered ends that agent's turn for the tick: the
// attempted rollout records the failure and the agent's other rollouts stay
// untouched. An answer, 4xx and 5xx included, does not end it.
func TestUnreachableAgentSkipsRemainingRollouts(t *testing.T) {
	install, uninstall, observe := enum.RolloutIntentInstall, enum.RolloutIntentUninstall, enum.RolloutIntentNoop
	for _, tc := range []struct {
		name    string
		intent  enum.RolloutIntent
		err     error
		skipped bool
	}{
		{"apply got no response", install, agentErr{errkind.Internal, fmt.Errorf("%w: %w: dial tcp 10.0.0.7:8085: connect: connection refused", proxy.ErrApplyTask, proxy.ErrRequest)}, true},
		{"apply deadline exceeded", install, agentErr{errkind.DeadlineExceeded, fmt.Errorf("%w: rpc error: code = DeadlineExceeded", proxy.ErrApplyTask)}, true},
		{"apply context deadline exceeded", install, agentErr{errkind.Internal, fmt.Errorf("%w: %w", proxy.ErrApplyTask, context.DeadlineExceeded)}, true},
		{"apply connection refused", install, fmt.Errorf("%w: %w", proxy.ErrApplyTask, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), true},
		{"apply connection reset", install, fmt.Errorf("%w: %w", proxy.ErrApplyTask, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}), true},
		{"delete got no response", uninstall, agentErr{errkind.Internal, fmt.Errorf("%w: %w: EOF", proxy.ErrDeleteTask, proxy.ErrRequest)}, true},
		{"observation got no response", observe, agentErr{errkind.Internal, fmt.Errorf("%w: %w: EOF", proxy.ErrGetTask, proxy.ErrRequest)}, true},
		{"apply answered 503", install, httpAnswer(proxy.ErrApplyTask, 503, errkind.Unavailable), false},
		{"apply answered 500", install, httpAnswer(proxy.ErrApplyTask, 500, errkind.Internal), false},
		{"apply answered 404", install, httpAnswer(proxy.ErrApplyTask, 404, errkind.NotFound), false},
		{"apply answered 409", install, httpAnswer(proxy.ErrApplyTask, 409, errkind.Conflict), false},
		{"apply answered grpc resource exhausted", install, agentErr{errkind.Unavailable, fmt.Errorf("%w: rpc error: code = ResourceExhausted desc = retained task limit reached", proxy.ErrApplyTask)}, false},
		{"apply answer malformed", install, agentErr{errkind.Internal, fmt.Errorf("%w: %w: unexpected end of JSON input", proxy.ErrApplyTask, proxy.ErrDecode)}, false},
		{"delete answered 500", uninstall, httpAnswer(proxy.ErrDeleteTask, 500, errkind.Internal), false},
		{"observation answered 500", observe, httpAnswer(proxy.ErrGetTask, 500, errkind.Internal), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProxy{
				applyErr:  []error{tc.err, tc.err, tc.err},
				deleteErr: []error{tc.err, tc.err, tc.err},
				getErr:    tc.err,
			}
			r, s, _ := newFakeRunner(t, fp)
			seeded := seedRollouts(t, s, "agent-a", 3, tc.intent)

			r.tick(context.Background())

			calls := len(fp.applies) + len(fp.deletes) + len(fp.gets)
			want := len(seeded)
			if tc.skipped {
				want = 1
			}
			if calls != want {
				t.Fatalf("agent got %d calls, want %d", calls, want)
			}
			// A failed push records the attempt; a failed observation records nothing.
			wantFailed := calls
			if tc.intent == observe {
				wantFailed = 0
			}
			failed, untouched := 0, 0
			for _, ro := range seeded {
				after, err := s.GetRollout(context.Background(), ro.ID())
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case after.UpdatedAt().Equal(ro.UpdatedAt()) && after.Status() == ro.Status() && after.Attempts() == ro.Attempts():
					untouched++
				case after.Status() == enum.SyncStatusFailed && after.Attempts() == ro.Attempts()+1:
					failed++
				}
			}
			if failed != wantFailed || untouched != len(seeded)-wantFailed {
				t.Fatalf("failed=%d untouched=%d, want %d and %d", failed, untouched, wantFailed, len(seeded)-wantFailed)
			}
		})
	}
}

// After the push deadline, a failed call counts as unanswered unless it carries
// the agent's answer.
func TestUnreachableAfterPushDeadline(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"response body cut by the deadline", agentErr{errkind.Internal, fmt.Errorf("%w: %w: context deadline exceeded", proxy.ErrApplyTask, proxy.ErrDecode)}, true},
		{"answer at the deadline", httpAnswer(proxy.ErrApplyTask, 503, errkind.Unavailable), false},
		{"success at the deadline", nil, false},
	} {
		if got := unreachable(expired, tc.err); got != tc.want {
			t.Errorf("%s: unreachable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Each of an agent's rollouts gets a full PushTimeout, not what the previous one left.
func TestEachRolloutGetsItsOwnPushTimeout(t *testing.T) {
	ap := &deadlineProxy{fakeProxy: fakeProxy{applyResp: []string{"t-0", "t-1"}}}
	r, s, _ := newFakeRunner(t, &fakeProxy{})
	r.pool = &fakePool{agents: map[string]proxy.AgentProxy{"http://agent-a": ap}}
	seedRollouts(t, s, "agent-a", 2, enum.RolloutIntentInstall)

	r.tick(context.Background())

	if len(ap.deadlines) != 2 {
		t.Fatalf("agent got %d applies, want 2", len(ap.deadlines))
	}
	if earliest := ap.returned[0].Add(r.cfg.PushTimeout); ap.deadlines[1].Before(earliest) {
		t.Fatalf("second rollout's deadline %v is before %v: it shares the first rollout's budget", ap.deadlines[1], earliest)
	}
}

// The tick pushes a failed rollout again only once its backoff has passed.
func TestTickWaitsOutBackoff(t *testing.T) {
	fp := &fakeProxy{applyResp: []string{"task"}}
	r, s, _ := newFakeRunner(t, fp)
	ro := seedRollouts(t, s, "agent-a", 1, enum.RolloutIntentInstall)[0]
	failedAt := func(at time.Time) {
		ro.RestoreState(enum.SyncStatusFailed, enum.RolloutIntentInstall, 0, 1, "offline", at, time.Time{})
		if err := s.UpsertRollout(context.Background(), ro); err != nil {
			t.Fatal(err)
		}
	}

	failedAt(time.Now())
	r.tick(context.Background())
	if len(fp.applies) != 0 {
		t.Fatal("failed rollout pushed again within its backoff")
	}

	failedAt(time.Now().Add(-r.cfg.TickInterval))
	r.tick(context.Background())
	if len(fp.applies) != 1 {
		t.Fatal("failed rollout not pushed once its backoff passed")
	}
}

// Each failure doubles the wait before the next push, from one tick up to five
// minutes. An exhausted rollout is never due; one that has not failed always is.
func TestRetryDueBacksOff(t *testing.T) {
	cfg := Config{MaxRetries: 10}.withDefaults()
	pushed := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	type probe struct {
		after time.Duration
		due   bool
	}
	for _, tc := range []struct {
		name     string
		status   enum.SyncStatus
		attempts int
		probes   []probe
	}{
		{"attempts 1: due after 10s", enum.SyncStatusFailed, 1, []probe{{10*time.Second - time.Nanosecond, false}, {10 * time.Second, true}}},
		{"attempts 3: due after 40s", enum.SyncStatusFailed, 3, []probe{{40*time.Second - time.Nanosecond, false}, {40 * time.Second, true}}},
		{"attempts 8: capped at 5m", enum.SyncStatusFailed, 8, []probe{{5*time.Minute - time.Nanosecond, false}, {5 * time.Minute, true}}},
		{"exhausted: never", enum.SyncStatusFailed, 10, []probe{{5 * time.Minute, false}, {24 * time.Hour, false}}},
		{"pending: always", enum.SyncStatusPending, 10, []probe{{-time.Hour, true}, {0, true}}},
		{"synced: always", enum.SyncStatusSynced, 10, []probe{{-time.Hour, true}, {0, true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ro, err := model.NewRollout("sp", "agent", 1)
			if err != nil {
				t.Fatal(err)
			}
			ro.RestoreState(tc.status, enum.RolloutIntentInstall, 0, tc.attempts, "", pushed, time.Time{})
			for _, p := range tc.probes {
				if got := retryDue(ro, pushed.Add(p.after), cfg); got != p.due {
					t.Fatalf("retryDue %v after the last push = %v, want %v", p.after, got, p.due)
				}
			}
		})
	}
}
