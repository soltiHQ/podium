package history

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster/standalone"
	"github.com/soltiHQ/control-plane/internal/proxy"
	historysvc "github.com/soltiHQ/control-plane/internal/service/history"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

func newRunner(t *testing.T, cfg Config) (*Runner, *inmemory.Store) {
	t.Helper()
	s := inmemory.New()
	svc, err := historysvc.New(s, historysvc.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool := proxy.NewPool(nil)
	t.Cleanup(func() { _ = pool.Close() })
	r, err := New(cfg, zerolog.Nop(), s, pool, svc, standalone.NewLeadership())
	if err != nil {
		t.Fatal(err)
	}
	return r, s
}

func seedAgent(t *testing.T, s *inmemory.Store, id, endpoint string, created time.Time) {
	t.Helper()
	a, err := model.NewAgentFrom(model.AgentParams{ID: id, Name: id, Endpoint: endpoint, EndpointType: 2, APIVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	a.SetCreatedAt(created)
	if err := s.UpsertAgent(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func seedRollout(t *testing.T, s *inmemory.Store, agent, spec string) {
	t.Helper()
	r, err := model.NewRollout(spec, agent, 99)
	if err != nil {
		t.Fatal(err)
	}
	r.SetActualTaskID("task")
	r.MarkSynced(99)
	if err := s.UpsertRollout(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func agentHandler(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/slow/") {
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/runs") {
		_, _ = fmt.Fprint(w, `{"metadata":{"taskUid":"uid","resourceVersion":"rv"},"runs":[{"generation":1,"attempt":1,"phase":"succeeded","startedAt":"2026-10-02T00:00:00Z","finishedAt":"2026-10-02T00:00:01Z","workload":{"apiVersion":"solti.io/v1","kind":"SubprocessTask"}}]}`)
	} else {
		_, _ = fmt.Fprint(w, `{"metadata":{"name":"task","uid":"uid","creationTimestamp":"2026-10-02T00:00:00Z"}}`)
	}
}

func TestSlowAgentsCannotStarveLaterQueuedAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(agentHandler))
	defer server.Close()
	r, s := newRunner(t, Config{CycleTimeout: 50 * time.Millisecond, MaxConcurrency: 1})
	base := time.Now()
	for i, id := range []string{"slow-first", "slow-second", "fast-third"} {
		endpoint := server.URL + "/slow"
		if i == 2 {
			endpoint = server.URL + "/fast"
		}
		seedAgent(t, s, id, endpoint, base.Add(-time.Duration(i)*time.Second))
		seedRollout(t, s, id, "spec")
	}
	r.tick(context.Background())
	if len(r.pendingAgents) == 0 {
		t.Fatal("cycle exhausted but no later agent retained")
	}
	r.tick(context.Background())
	r.tick(context.Background())
	rows, err := storage.AllExecutions(context.Background(), s, nil)
	if err != nil || len(rows) != 1 || rows[0].AgentID() != "fast-third" {
		t.Fatalf("later agent starved rows=%v err=%v", rows, err)
	}
	if rows[0].Generation() != 1 {
		t.Fatal("control-plane generation99 replaced SDK runtime generation1")
	}
}

func TestRetentionRunsBeforeSlowSourceConsumesCycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(agentHandler))
	defer server.Close()
	r, s := newRunner(t, Config{CycleTimeout: 30 * time.Millisecond, MaxConcurrency: 1})
	record, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "old", TaskName: "task", TaskUID: "u", Generation: 1, Attempt: 1, Status: "failed", CapturedAt: time.Now().Add(-14 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertExecution(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	seedAgent(t, s, "slow", server.URL+"/slow", time.Now())
	seedRollout(t, s, "slow", "spec")
	r.tick(context.Background())
	rows, err := storage.AllExecutions(context.Background(), s, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("source timeout defeated archive retention rows=%d err=%v", len(rows), err)
	}
}

func TestWorkStateOnlyRetainsUnfinishedPagesAndSweepsDeletedAgents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(agentHandler))
	defer server.Close()
	r, s := newRunner(t, Config{PageSize: 1, MaxAgentsPerCycle: 1})
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("empty-%d", i)
		seedAgent(t, s, id, server.URL, time.Now())
		r.collectAgent(context.Background(), id)
		if err := s.DeleteAgent(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.work) != 0 || len(r.maintenance) != 0 {
		t.Fatal("empty agents left cached pages")
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("partial-%d", i)
		seedAgent(t, s, id, server.URL, time.Now())
		seedRollout(t, s, id, "first")
		seedRollout(t, s, id, "second")
		r.collectAgent(context.Background(), id)
	}
	if len(r.work) != 3 || len(r.maintenance) != 3 {
		t.Fatal("partial continuations were not retained")
	}
	liveCursor := r.work["partial-2"].cursor
	if err := s.DeleteAgent(context.Background(), "partial-0"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAgent(context.Background(), "partial-1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		r.pruneWork(context.Background())
	}
	if len(r.work) != 1 || len(r.maintenance) != 1 || r.work["partial-2"].cursor != liveCursor {
		t.Fatalf("deleted cache cleanup lost active continuation work=%d queue=%d", len(r.work), len(r.maintenance))
	}
	r.collectAgent(context.Background(), "partial-2")
	if len(r.work) != 0 {
		t.Fatal("completed final page left persistent state")
	}
}

func TestCollectorDefaultsAndInvalidBudgets(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{{TickInterval: -time.Second}, {CycleTimeout: -time.Second}, {MaxConcurrency: -1}, {MaxConcurrency: 33}, {PageSize: 1001}, {MaxPages: 65}, {MaxAgentsPerCycle: 501}} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid configuration accepted %+v", cfg)
		}
	}
}

func TestCompletedRecreatedWorkDoesNotAttachStaleMaintenanceEntry(t *testing.T) {
	r, s := newRunner(t, Config{})
	seedAgent(t, s, "agent", "http://127.0.0.1", time.Now())
	old := &agentWork{cursor: "old"}
	r.work["agent"] = old
	r.retainWork("agent", old)
	old.cursor = ""
	r.retainWork("agent", old)
	current := &agentWork{cursor: "new"}
	r.work["agent"] = current
	r.retainWork("agent", current)
	r.pruneWork(context.Background())
	if len(r.maintenance) != 1 || r.maintenance[0].work != current || r.work["agent"] != current {
		t.Fatalf("stale maintenance attached to replacement: %d", len(r.maintenance))
	}
	for i := 0; i < 3; i++ {
		r.pruneWork(context.Background())
	}
	if len(r.maintenance) != 1 {
		t.Fatal("queue duplicated across sweeps")
	}
}

func TestCollectorResumesStaticHistoryWithOneRowOnePageBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/runs") {
			agentHandler(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Query().Get("continue") == "next" {
			_, _ = fmt.Fprint(w, `{"metadata":{"taskUid":"uid","resourceVersion":"rv"},"runs":[{"generation":1,"attempt":2,"phase":"failed","startedAt":"2026-10-02T00:00:00Z","finishedAt":"2026-10-02T00:00:01Z"}]}`)
		} else {
			_, _ = fmt.Fprint(w, `{"metadata":{"taskUid":"uid","resourceVersion":"rv","continue":"next"},"runs":[{"generation":1,"attempt":1,"phase":"succeeded","startedAt":"2026-10-02T00:00:00Z","finishedAt":"2026-10-02T00:00:01Z"}]}`)
		}
	}))
	defer server.Close()
	r, s := newRunner(t, Config{PageSize: 1, MaxPages: 1})
	seedAgent(t, s, "agent", server.URL, time.Now())
	seedRollout(t, s, "agent", "spec")
	r.tick(context.Background())
	if len(r.progress) != 1 {
		t.Fatal("partial run cursor discarded")
	}
	r.tick(context.Background())
	rows, err := storage.AllExecutions(context.Background(), s, nil)
	if err != nil || len(rows) != 2 || len(r.progress) != 1 || !r.progress[model.RolloutID("spec", "agent")].complete {
		t.Fatalf("bounded collector permanently repeated prefix: rows%d progress%d err%v", len(rows), len(r.progress), err)
	}
}

func TestRunContinuationCacheHasHardBoundAndRemovesOrphans(t *testing.T) {
	r, _ := newRunner(t, Config{MaxRunContinuations: 2, MaxAgentsPerCycle: 1})
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("missing-%d", i)
		r.rememberProgress(id, historysvc.CaptureRequest{AgentID: "a", TaskName: "task"}, &historysvc.RunContinuation{TaskUID: "uid", ResourceVersion: "rv", Cursor: "next"})
	}
	if len(r.progress) != 2 {
		t.Fatal("continuation cache exceeded cap")
	}
	for i := 0; i < 2; i++ {
		r.pruneProgress(context.Background())
	}
	if len(r.progress) != 0 {
		t.Fatal("orphan continuations retained")
	}
}

func TestSlowFirstAgentCannotConsumeSecondAgentsSlotAfterDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(agentHandler))
	defer server.Close()
	r, s := newRunner(t, Config{CycleTimeout: 40 * time.Millisecond, MaxConcurrency: 1})
	now := time.Now()
	seedAgent(t, s, "slow", server.URL+"/slow", now)
	seedRollout(t, s, "slow", "spec")
	seedAgent(t, s, "fast", server.URL+"/fast", now.Add(-time.Second))
	seedRollout(t, s, "fast", "spec")
	r.tick(context.Background())
	if len(r.pendingAgents) != 1 || r.pendingAgents[0].ID() != "fast" {
		t.Fatalf("unstarted agent dequeued on expired slot: queued=%d", len(r.pendingAgents))
	}
	r.tick(context.Background())
	rows, err := storage.AllExecutions(context.Background(), s, nil)
	if err != nil || len(rows) != 1 || rows[0].AgentID() != "fast" {
		t.Fatalf("fast second agent starved rows=%d err=%v", len(rows), err)
	}
}
