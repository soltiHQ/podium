package sync

import (
	"context"
	"slices"
	gosync "sync"
	"testing"
	"time"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// turnWait bounds how long a test waits for the runner. A tick of fakes takes
// milliseconds: the bound only turns a hang into a failure.
const turnWait = 10 * time.Second

// testClock is a runner clock that only the test moves. The runner's workers
// may read it while the test moves it.
type testClock struct {
	mu  gosync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// slowAgent answers each observation, but only when the test lets it: the
// call reports its task on entered, then waits for answer. The test moves the
// runner's clock while a call waits, so on that clock the call takes as long
// as the test says.
type slowAgent struct {
	fakeProxy
	entered chan string
	answer  chan struct{}
}

func (a *slowAgent) GetTask(ctx context.Context, id string) (*proxyv1.GetTaskResponse, error) {
	select {
	case a.entered <- id:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-a.answer:
		return &proxyv1.GetTaskResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// slowTick runs one tick. Once the other agents have made calls calls, it
// answers each call of slow after moving clock by delay, and returns the tasks
// slow was asked about, in order. Waiting for the other agents first keeps the
// moving clock out of their turns.
func slowTick(t *testing.T, r *Runner, slow *slowAgent, clock *testClock, delay time.Duration, others <-chan struct{}, calls int) []string {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.tick(context.Background())
	}()
	for range calls {
		select {
		case <-others:
		case <-time.After(turnWait):
			t.Fatal("the other agents were not called")
		}
	}
	var asked []string
	for {
		select {
		case id := <-slow.entered:
			asked = append(asked, id)
			clock.advance(delay)
			select {
			case slow.answer <- struct{}{}:
			case <-time.After(turnWait):
				t.Fatal("the slow agent's call stopped waiting for its answer")
			}
		case <-done:
			return asked
		case <-time.After(turnWait):
			t.Fatal("tick did not finish")
		}
	}
}

// An agent that answers slowly, but within push_timeout, gets push_timeout of
// wall time per tick: the tick ends within about one push_timeout, the other
// agents finish their work in it, and what is left of the slow agent's work
// waits for the next tick, where its observations go on from where they
// stopped. Every synced rollout of the slow agent is observed within two ticks.
func TestSlowAgentTurnIsBounded(t *testing.T) {
	const delay = 3 * time.Second
	clock := &testClock{now: time.Now()}
	others := make(chan struct{}, 2)
	called := func() { others <- struct{}{} }
	fast := &fakeProxy{applyResp: []string{"fast-0", "fast-1"}, onApply: called, onGet: called}
	slow := &slowAgent{entered: make(chan string), answer: make(chan struct{})}
	r, s, _ := newFakeRunner(t, &fakeProxy{})
	r.clock = clock.Now
	r.pool = &fakePool{agents: map[string]proxy.AgentProxy{"http://agent-slow": slow, "http://agent-fast": fast}}
	synced := seedRollouts(t, s, "agent-slow", 10, enum.RolloutIntentNoop) // 30s of calls
	pending := seedRollouts(t, s, "agent-fast", 2, enum.RolloutIntentInstall)

	start := clock.Now()
	first := slowTick(t, r, slow, clock, delay, others, len(pending))
	if took := clock.Now().Sub(start); took > r.cfg.PushTimeout+delay {
		t.Fatalf("tick took %v on the runner clock, want at most push_timeout (%v) plus one call (%v)", took, r.cfg.PushTimeout, delay)
	}
	if want := int(r.cfg.PushTimeout / delay); len(first) != want {
		t.Fatalf("slow agent got %d calls in one tick, want %d: calls start until push_timeout is used", len(first), want)
	}
	for _, ro := range pending {
		after, err := s.GetRollout(context.Background(), ro.ID())
		if err != nil || after.Status() != enum.SyncStatusSynced {
			t.Fatalf("rollout %s of the fast agent not completed in the tick (err=%v)", ro.ID(), err)
		}
	}
	// Leftover work is no failure: it records nothing, and the agent that
	// answered is not backed off.
	for _, ro := range synced {
		after, err := s.GetRollout(context.Background(), ro.ID())
		if err != nil {
			t.Fatal(err)
		}
		if after.Status() != enum.SyncStatusSynced || !after.UpdatedAt().Equal(ro.UpdatedAt()) {
			t.Fatalf("rollout %s of the slow agent changed: status=%s", ro.ID(), after.Status())
		}
	}
	if b, ok := r.backoffs["agent-slow"]; ok {
		t.Fatalf("slow agent backed off: %+v", b)
	}

	second := slowTick(t, r, slow, clock, delay, others, len(pending))
	observed := make(map[string]bool)
	for _, task := range append(first, second...) {
		observed[task] = true
	}
	for _, ro := range synced {
		if !observed[ro.ActualTaskID()] {
			t.Fatalf("task %s of the slow agent not observed in two ticks: first %v, second %v", ro.ActualTaskID(), first, second)
		}
	}
}

// specOf returns the spec ID each pushed manifest carries.
func specOf(applies []*taskv1.TaskManifest) []string {
	out := make([]string, 0, len(applies))
	for _, m := range applies {
		out = append(out, m.GetMetadata().GetAnnotations()["podium.solti.io/spec-id"])
	}
	return out
}

// An agent's pushes go least recently attempted first: those never attempted,
// then by their last push, oldest first. Listing order, newest rollout first,
// would put them the other way round.
func TestPushesLeastRecentlyAttemptedFirst(t *testing.T) {
	ctx := context.Background()
	fp := &fakeProxy{}
	r, s, _ := newFakeRunner(t, fp)
	now := time.Now()
	r.clock = func() time.Time { return now }
	for i, tc := range []struct {
		specID   string
		pushedAt time.Time // zero: never attempted
	}{
		{"sp-never", time.Time{}},
		{"sp-oldest", now.Add(-time.Hour)},
		{"sp-middle", now.Add(-30 * time.Minute)},
		{"sp-recent", now.Add(-time.Minute)},
	} {
		ro := seedRollout(t, s, tc.specID, "agent-a", enum.RolloutIntentInstall, "")
		if !tc.pushedAt.IsZero() {
			ro.RestoreState(enum.SyncStatusFailed, enum.RolloutIntentInstall, 0, 1, "offline", tc.pushedAt, time.Time{})
		}
		ro.SetCreatedAt(now.Add(time.Duration(i-10) * time.Hour)) // listed newest first
		if err := s.UpsertRollout(ctx, ro); err != nil {
			t.Fatal(err)
		}
	}

	r.tick(ctx)

	want := []string{"sp-never", "sp-oldest", "sp-middle", "sp-recent"}
	if got := specOf(fp.applies); !slices.Equal(got, want) {
		t.Fatalf("pushes went %v, want %v", got, want)
	}
}

// changeAgent applies change to the stored row of agent id.
func changeAgent(t *testing.T, s storage.Storage, id string, change func(*model.Agent)) {
	t.Helper()
	ag, err := s.GetAgent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	change(ag)
	if err = s.UpsertAgent(context.Background(), ag); err != nil {
		t.Fatal(err)
	}
}

// agentRow sets the status and uptime of an agent row.
func agentRow(status enum.AgentStatus, uptime int64) func(*model.Agent) {
	return func(ag *model.Agent) {
		ag.SetStatus(status)
		ag.SetUptimeSeconds(uptime)
	}
}

// An agent row that shows a reconnect since the agent's backoff started (the
// agent is active again after it was not, or its uptime went down) clears the
// backoff: the next tick calls the agent. An agent that stays active keeps its
// backoff: it may answer discovery but be unreachable from podium (NAT).
func TestAgentBackoffClearsOnReconnect(t *testing.T) {
	active, inactive, disconnected := enum.AgentStatusActive, enum.AgentStatusInactive, enum.AgentStatusDisconnected
	for _, tc := range []struct {
		name   string
		before func(*model.Agent) // the row when the backoff starts
		after  func(*model.Agent) // the row on the next tick
		called bool
	}{
		{"uptime decreased", agentRow(active, 100), agentRow(active, 5), true},
		{"active again after inactive", agentRow(inactive, 100), agentRow(active, 400), true},
		{"active again after disconnected", agentRow(disconnected, 100), agentRow(active, 400), true},
		{"stays active", agentRow(active, 100), agentRow(active, 130), false},
		{"inactive since", agentRow(active, 100), agentRow(inactive, 100), false},
		{"stays disconnected", agentRow(disconnected, 100), agentRow(disconnected, 100), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProxy{getErr: noAnswer(proxy.ErrGetTask)}
			r, s, _ := newFakeRunner(t, fp)
			start := time.Now()
			now := start
			r.clock = func() time.Time { return now }
			_ = seedRollout(t, s, "sp-live", "agent-a", enum.RolloutIntentNoop, "task-live")
			changeAgent(t, s, "agent-a", tc.before)

			r.tick(context.Background()) // unanswered: backoff 10s
			changeAgent(t, s, "agent-a", tc.after)
			now = start.Add(time.Second)
			r.tick(context.Background())

			want := 1
			if tc.called {
				want = 2
			}
			if len(fp.gets) != want {
				t.Fatalf("agent observed %d times, want %d", len(fp.gets), want)
			}
			// A cleared backoff starts over: the call after the reconnect,
			// unanswered again, is the first failure, not the second.
			if b := r.backoffs["agent-a"]; tc.called && b.failures != 1 {
				t.Fatalf("backoff after the reconnect counts %d failures, want 1", b.failures)
			}
		})
	}
}

// An agent whose row is gone when its backoff starts (lifecycle deleted it
// during the call) counts as not active, as a new agent does for Upsert: the
// agent syncing again is a reconnect and clears the backoff.
func TestAgentBackoffClearsWhenADeletedAgentSyncsAgain(t *testing.T) {
	ctx := context.Background()
	fp := &fakeProxy{getErr: noAnswer(proxy.ErrGetTask)}
	r, s, _ := newFakeRunner(t, fp)
	start := time.Now()
	now := start
	r.clock = func() time.Time { return now }
	_ = seedRollout(t, s, "sp-live", "agent-a", enum.RolloutIntentNoop, "task-live")
	row, err := s.GetAgent(ctx, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	fp.onGet = func() {
		if err := s.DeleteAgent(ctx, "agent-a"); err != nil {
			t.Error(err)
		}
	}

	r.tick(ctx) // unanswered, and the row is gone: backoff 10s
	fp.onGet = nil
	if err = s.UpsertAgent(ctx, row); err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Second)
	r.tick(ctx)

	if len(fp.gets) != 2 {
		t.Fatalf("agent observed %d times, want 2: syncing again after deletion is a reconnect", len(fp.gets))
	}
}

// Each new backoff window records the agent row again. An agent down long
// enough to be marked inactive is inactive in the row of its latest window,
// so it syncing again clears the backoff, though its uptime only grew (a
// network outage, not a restart).
func TestAgentBackoffRecordsTheRowOfEachWindow(t *testing.T) {
	fp := &fakeProxy{getErr: noAnswer(proxy.ErrGetTask)}
	r, s, _ := newFakeRunner(t, fp)
	start := time.Now()
	now := start
	r.clock = func() time.Time { return now }
	_ = seedRollout(t, s, "sp-live", "agent-a", enum.RolloutIntentNoop, "task-live")
	changeAgent(t, s, "agent-a", agentRow(enum.AgentStatusActive, 100))

	r.tick(context.Background()) // unanswered: backoff 10s
	changeAgent(t, s, "agent-a", agentRow(enum.AgentStatusInactive, 100))
	now = start.Add(10 * time.Second)
	r.tick(context.Background()) // unanswered again: backoff 20s
	changeAgent(t, s, "agent-a", agentRow(enum.AgentStatusActive, 400))
	now = start.Add(11 * time.Second)
	r.tick(context.Background())

	if len(fp.gets) != 3 {
		t.Fatalf("agent observed %d times, want 3: the agent that came back must be called", len(fp.gets))
	}
}
