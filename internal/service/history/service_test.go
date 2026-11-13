package history

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

type observedSource struct {
	uid    string
	pages  map[string]*proxy.TaskRunPage
	onPage func(context.Context, string) (*proxy.TaskRunPage, error)
}

func (f *observedSource) GetTask(context.Context, string) (*proxyv1.GetTaskResponse, error) {
	return &proxyv1.GetTaskResponse{Task: &proxyv1.Task{ID: "task", UID: f.uid}}, nil
}

func (f *observedSource) ListTaskRunsPage(ctx context.Context, _ string, q proxy.TaskRunQuery) (*proxy.TaskRunPage, error) {
	if f.onPage != nil {
		return f.onPage(ctx, q.Cursor)
	}
	return f.pages[q.Cursor], nil
}

func (*observedSource) ListTasks(context.Context, proxy.TaskFilter) (*proxyv1.ListTasksResponse, error) {
	return nil, nil
}

func (*observedSource) ApplyTask(context.Context, proxy.TaskSubmission) (string, error) {
	return "", nil
}

func (*observedSource) ListTaskRuns(context.Context, string) (*proxyv1.ListTaskRunsResponse, error) {
	return nil, nil
}
func (*observedSource) DeleteTask(context.Context, string) error { return nil }
func (*observedSource) StreamTaskLogs(context.Context, string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
	return nil, nil
}

// counting wraps a store, or the handle of one of its transactions, and counts the
// passes over its rows: every Scan* and List* call, which is also what the
// storage.All* helpers come to. Like the stores it wraps it is a storage.Scanner,
// so a scan stays one pass.
type counting struct {
	storage.Storage
	scans *atomic.Int64
}

func (c counting) ScanAgents(ctx context.Context, f storage.AgentFilter) ([]*model.Agent, error) {
	c.scans.Add(1)
	return storage.AllAgents(ctx, c.Storage, f)
}

func (c counting) ScanRollouts(ctx context.Context, f storage.RolloutFilter) ([]*model.Rollout, error) {
	c.scans.Add(1)
	return storage.AllRollouts(ctx, c.Storage, f)
}

func (c counting) ScanSpecs(ctx context.Context, f storage.SpecFilter) ([]*model.Spec, error) {
	c.scans.Add(1)
	return storage.AllSpecs(ctx, c.Storage, f)
}

func (c counting) ScanExecutions(ctx context.Context, f storage.ExecutionFilter) ([]*model.ExecutionRecord, error) {
	c.scans.Add(1)
	return storage.AllExecutions(ctx, c.Storage, f)
}

func (c counting) ListAgents(ctx context.Context, f storage.AgentFilter, o storage.ListOptions) (*storage.AgentListResult, error) {
	c.scans.Add(1)
	return c.Storage.ListAgents(ctx, f, o)
}

func (c counting) ListRollouts(ctx context.Context, f storage.RolloutFilter, o storage.ListOptions) (*storage.RolloutListResult, error) {
	c.scans.Add(1)
	return c.Storage.ListRollouts(ctx, f, o)
}

func (c counting) ListSpecs(ctx context.Context, f storage.SpecFilter, o storage.ListOptions) (*storage.SpecListResult, error) {
	c.scans.Add(1)
	return c.Storage.ListSpecs(ctx, f, o)
}

func (c counting) ListExecutions(ctx context.Context, f storage.ExecutionFilter, o storage.ListOptions) (*storage.ExecutionListResult, error) {
	c.scans.Add(1)
	return c.Storage.ListExecutions(ctx, f, o)
}

// countedStore counts the transactions opened and the passes over the rows made
// inside them or outside, and fails transactions on demand.
type countedStore struct {
	counting
	transactions int
	failure      error // fails each transaction before it runs
	landed       error // fails each transaction after it committed, as a timeout does whose write still lands
}

func (s *countedStore) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	s.transactions++
	if s.failure != nil {
		return s.failure
	}
	err := s.Storage.WithTx(ctx, func(tx storage.Storage) error { return fn(counting{tx, s.scans}) })
	if err == nil && s.landed != nil {
		return s.landed
	}
	return err
}

// unreadable is a store that fails to list its executions while err is set. It is
// no storage.Scanner, so the passes the service makes over it are listings.
type unreadable struct {
	storage.Storage
	err error
}

func (u *unreadable) ListExecutions(ctx context.Context, f storage.ExecutionFilter, o storage.ListOptions) (*storage.ExecutionListResult, error) {
	if u.err != nil {
		return nil, u.err
	}
	return u.Storage.ListExecutions(ctx, f, o)
}

func newService(t *testing.T, cfg Config) (*Service, *countedStore) {
	t.Helper()
	store := &countedStore{counting: counting{Storage: inmemory.New(), scans: new(atomic.Int64)}}
	svc, err := New(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }
	return svc, store
}

func run(attempt int, status string) proxyv1.TaskRun {
	return proxyv1.TaskRun{Generation: 3, Attempt: attempt, Status: status, StartedAt: 40, FinishedAt: 20, FinishedAtPresent: true, WorkloadAPIVersion: "solti.io/v1", WorkloadKind: "SubprocessTask"}
}

func source(runs ...proxyv1.TaskRun) *observedSource {
	return &observedSource{uid: "uid", pages: map[string]*proxy.TaskRunPage{"": {TaskUID: "uid", ResourceVersion: "opaque-rv", Runs: runs}}}
}

func captureReq() CaptureRequest {
	return CaptureRequest{AgentID: "agent", AgentName: "Agent", TaskName: "task", SpecID: "spec", SpecName: "Spec"}
}

func all(t *testing.T, s storage.Storage) []*model.ExecutionRecord {
	t.Helper()
	rows, err := storage.AllExecutions(context.Background(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCaptureArchivesTerminalSnapshotOnceWithUIDAndRuntimeGeneration(t *testing.T) {
	svc, store := newService(t, Config{})
	zero := int32(0)
	success := run(1, "succeeded")
	success.ExitCode = &zero
	pending := run(2, "running")
	pending.FinishedAtPresent = false
	result, err := svc.Capture(context.Background(), captureReq(), source(success, pending, run(3, "canceled")), CaptureOptions{})
	if err != nil || result.Added != 2 || result.Observed != 2 || result.Partial || store.transactions != 1 {
		t.Fatalf("capture=%+v err=%v tx=%d", result, err, store.transactions)
	}
	rows := all(t, store)
	if len(rows) != 2 {
		t.Fatal(len(rows))
	}
	for _, row := range rows {
		if row.TaskUID() != "uid" || row.Generation() != 3 || row.SpecID() != "spec" || row.SourceResourceVersion() != "opaque-rv" || row.WorkloadKind() != "SubprocessTask" || row.FinishedAtMs() != 20 || row.StartedAtMs() != 40 {
			t.Fatalf("lost source semantics %+v", row.Params())
		}
		if row.Attempt() == 1 && (row.ExitCode() == nil || *row.ExitCode() != 0) {
			t.Fatal("lost present zero exit")
		}
	}
	first := rows[0].Params()
	svc.now = func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) }
	result, err = svc.Capture(context.Background(), captureReq(), source(success, pending, run(3, "canceled")), CaptureOptions{})
	if err != nil || result.Added != 0 || result.Existing != 2 || store.transactions != 1 {
		t.Fatalf("repeat rewrote archive %+v %v tx=%d", result, err, store.transactions)
	}
	got, _ := store.GetExecution(context.Background(), rows[0].ID())
	if !got.CapturedAt().Equal(first.CapturedAt) {
		t.Fatal("first observation renewed")
	}
	if pruned, err := svc.Prune(context.Background()); err != nil || pruned != 0 || store.transactions != 1 {
		t.Fatal("idle retention wrote a snapshot")
	}
}

func TestCaptureRejectsMixedSnapshotWithoutPublishingAnyRows(t *testing.T) {
	cases := map[string]*observedSource{
		"uid":              {uid: "uid", pages: map[string]*proxy.TaskRunPage{"": {TaskUID: "uid", ResourceVersion: "one", NextCursor: "next", Runs: []proxyv1.TaskRun{run(1, "failed")}}, "next": {TaskUID: "replacement", ResourceVersion: "one", Runs: []proxyv1.TaskRun{run(2, "succeeded")}}}},
		"version":          {uid: "uid", pages: map[string]*proxy.TaskRunPage{"": {TaskUID: "uid", ResourceVersion: "one", NextCursor: "next", Runs: []proxyv1.TaskRun{run(1, "failed")}}, "next": {TaskUID: "uid", ResourceVersion: "two", Runs: []proxyv1.TaskRun{run(2, "succeeded")}}}},
		"repeated-cursor":  {uid: "uid", pages: map[string]*proxy.TaskRunPage{"": {TaskUID: "uid", ResourceVersion: "one", NextCursor: "next", Runs: []proxyv1.TaskRun{run(1, "failed")}}, "next": {TaskUID: "uid", ResourceVersion: "one", NextCursor: "next", Runs: []proxyv1.TaskRun{run(2, "succeeded")}}}},
		"repeated-attempt": source(run(1, "failed"), run(1, "failed")),
		"missing-rv":       {uid: "uid", pages: map[string]*proxy.TaskRunPage{"": {TaskUID: "uid", Runs: []proxyv1.TaskRun{run(1, "failed")}}}},
	}
	missingFinish := run(1, "failed")
	missingFinish.FinishedAtPresent = false
	cases["missing-finish"] = source(missingFinish)
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			svc, s := newService(t, Config{})
			_, err := svc.Capture(context.Background(), captureReq(), src, CaptureOptions{})
			if err == nil || s.transactions != 0 || len(all(t, s)) != 0 {
				t.Fatalf("published invalid snapshot err=%v tx=%d", err, s.transactions)
			}
		})
	}
}

// A run whose status is no phase that podium archives is refused with an error
// that names the status, quoted and cut by proxy.CapAgentText, and the history
// runner logs that error: so the log holds at most MaxAgentTextBytes of the status
// and the marker, whatever the pager answered. The HTTP proxy refuses a phase over
// MaxRunPhaseBytes before that. Nothing is archived.
func TestUnknownRunStatusIsCutInTheError(t *testing.T) {
	const prefix = "history: unknown source run status "
	for _, tc := range []struct {
		name, status string
		cut          bool
	}{
		{"a short status", "paused", false},
		{"a status of 1 MiB", strings.Repeat("x", 1<<20), true},
		{"a status of 1 MiB that quoting escapes", strings.Repeat("\x00", 1<<20), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, s := newService(t, Config{})
			_, err := svc.Capture(context.Background(), captureReq(), source(run(1, tc.status)), CaptureOptions{})
			if err == nil || s.transactions != 0 || len(all(t, s)) != 0 {
				t.Fatalf("archived a run of an unknown status: err=%.100v tx=%d", err, s.transactions)
			}
			text, ok := strings.CutPrefix(err.Error(), prefix)
			if !ok {
				t.Fatalf("error %.100q, want it to start with %q", err, prefix)
			}
			if !tc.cut {
				if text != strconv.Quote(tc.status) {
					t.Fatalf("error %q, want the status quoted whole", err)
				}
				return
			}
			if len(text) > proxy.MaxAgentTextBytes+len("…") || !strings.HasSuffix(text, "…") {
				t.Fatalf("the error holds %d bytes of the status, want at most %d and the marker", len(text), proxy.MaxAgentTextBytes)
			}
		})
	}
}

func TestCanceledSourceAndFailedTransactionDoNotPublish(t *testing.T) {
	svc, s := newService(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	src := source()
	src.onPage = func(ctx context.Context, _ string) (*proxy.TaskRunPage, error) {
		cancel()
		return &proxy.TaskRunPage{TaskUID: "uid", ResourceVersion: "rv", Runs: []proxyv1.TaskRun{run(1, "failed")}}, nil
	}
	if _, err := svc.Capture(ctx, captureReq(), src, CaptureOptions{}); !errors.Is(err, context.Canceled) || s.transactions != 0 || len(all(t, s)) != 0 {
		t.Fatalf("canceled source published: %v", err)
	}
	s.failure = errors.New("store commit failed")
	result, err := svc.Capture(context.Background(), captureReq(), source(run(1, "failed")), CaptureOptions{})
	if err == nil || result.Added != 0 || len(all(t, s)) != 0 {
		t.Fatalf("failed commit reported success %+v %v", result, err)
	}
}

func TestPageAndByteBoundsReportHonestPartialCapture(t *testing.T) {
	for _, bound := range []string{"pages", "bytes"} {
		t.Run(bound, func(t *testing.T) {
			svc, s := newService(t, Config{})
			src := source(run(1, "succeeded"))
			opts := CaptureOptions{}
			if bound == "pages" {
				src.pages[""].NextCursor = "next"
				opts.MaxPages = 1
			} else {
				src.pages[""].Runs = append(src.pages[""].Runs, run(2, "failed"))
				record, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "agent", AgentName: "Agent", TaskName: "task", TaskUID: "uid", Generation: 3, Attempt: 1, Status: "succeeded", StartedAtMs: 40, FinishedAtMs: 20, WorkloadAPIVersion: "solti.io/v1", WorkloadKind: "SubprocessTask", SpecID: "spec", SpecName: "Spec", SourceResourceVersion: "opaque-rv", CapturedAt: svc.now()})
				if err != nil {
					t.Fatal(err)
				}
				opts.MaxBytes = record.SizeBytes()
			}
			result, err := svc.Capture(context.Background(), captureReq(), src, opts)
			if err != nil || !result.Partial || result.Added != 1 || len(all(t, s)) != 1 {
				t.Fatalf("bound %+v %v", result, err)
			}
		})
	}
}

func TestRetentionUsesFirstCaptureAgeCountAndLogicalPayload(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for _, policy := range []string{"age", "count", "bytes"} {
		t.Run(policy, func(t *testing.T) {
			cfg := DefaultConfig()
			switch policy {
			case "age":
				cfg.Retention = time.Hour
			case "count":
				cfg.MaxRecords = 1
			}
			svc, s := newService(t, cfg)
			var records []*model.ExecutionRecord
			for i, offset := range []time.Duration{-2 * time.Hour, -time.Minute} {
				p := model.ExecutionRecordParams{AgentID: "a", TaskName: "t", TaskUID: "u", Generation: 1, Attempt: uint32(i + 1), Status: "failed", CapturedAt: now.Add(offset)}
				record, err := model.NewExecutionRecord(p)
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, record)
				if err := s.UpsertExecution(context.Background(), record); err != nil {
					t.Fatal(err)
				}
			}
			if policy == "bytes" {
				svc.cfg.MaxBytes = records[1].SizeBytes()
			}
			n, err := svc.Prune(context.Background())
			rows := all(t, s)
			if err != nil || n != 1 || len(rows) != 1 || rows[0].Attempt() != 2 {
				t.Fatalf("retention n=%d rows=%d err=%v", n, len(rows), err)
			}
			if _, err := svc.Prune(context.Background()); err != nil || s.transactions != 1 {
				t.Fatal("unchanged pruning opened another write")
			}
		})
	}
}

func TestNewInsertAndRetentionAreAtomic(t *testing.T) {
	svc, s := newService(t, Config{MaxRecords: 1})
	if _, err := svc.Capture(context.Background(), captureReq(), source(run(1, "failed")), CaptureOptions{}); err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC) }
	result, err := svc.Capture(context.Background(), captureReq(), source(run(2, "succeeded")), CaptureOptions{})
	rows := all(t, s)
	if err != nil || result.Added != 1 || result.Pruned != 1 || len(rows) != 1 || rows[0].Attempt() != 2 || s.transactions != 2 {
		t.Fatalf("atomic retention %+v rows=%d tx=%d err=%v", result, len(rows), s.transactions, err)
	}
}

func TestListIsOfflineAndValidatesBoundedCanonicalFilters(t *testing.T) {
	svc, s := newService(t, Config{})
	if _, err := svc.Capture(context.Background(), captureReq(), source(run(1, "failed")), CaptureOptions{}); err != nil {
		t.Fatal(err)
	}
	page, err := svc.List(context.Background(), ListQuery{Criteria: storage.ExecutionQueryCriteria{AgentID: "agent", TaskUID: "uid", Statuses: []string{"failed"}}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("offline history %v %v", page, err)
	}
	for _, criteria := range []storage.ExecutionQueryCriteria{{TaskName: strings.Repeat("x", 513)}, {TaskUID: string([]byte{0xff})}, {Statuses: []string{"running"}}} {
		if _, err := svc.List(context.Background(), ListQuery{Criteria: criteria}); !errors.Is(err, storage.ErrInvalidArgument) {
			t.Fatalf("bad filter accepted %v", err)
		}
	}
	if len(all(t, s)) != 1 {
		t.Fatal("query erased archived rows")
	}
}

func TestConfigRejectsNegativeRatherThanHidingIt(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{{Retention: -time.Second}, {MaxRecords: -1}, {MaxBytes: -1}} {
		if err := cfg.Validate(); err == nil {
			t.Fatal("negative configuration accepted")
		}
	}
}

func TestContinuationEventuallyArchivesAllStaticPages(t *testing.T) {
	svc, s := newService(t, Config{})
	src := source(run(1, "failed"))
	src.pages[""].NextCursor = "next"
	src.pages["next"] = &proxy.TaskRunPage{TaskUID: "uid", ResourceVersion: "opaque-rv", Runs: []proxyv1.TaskRun{run(2, "succeeded")}}
	opts := CaptureOptions{PageSize: 1, MaxPages: 1}
	first, err := svc.Capture(context.Background(), captureReq(), src, opts)
	if err != nil || !first.Partial || first.Continuation == nil || first.Continuation.Cursor != "next" {
		t.Fatalf("first page %+v %v", first, err)
	}
	opts.Continuation = first.Continuation
	second, err := svc.Capture(context.Background(), captureReq(), src, opts)
	if err != nil || second.Partial || second.Continuation != nil || second.Added != 1 || len(all(t, s)) != 2 {
		t.Fatalf("continuation starved %+v %v", second, err)
	}
}

func TestByteContinuationResumesInsidePageWithoutSkippingSuffix(t *testing.T) {
	svc, s := newService(t, Config{})
	src := source(run(1, "succeeded"), run(2, "succeeded"))
	record, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "agent", AgentName: "Agent", TaskName: "task", TaskUID: "uid", Generation: 3, Attempt: 1, Status: "succeeded", StartedAtMs: 40, FinishedAtMs: 20, WorkloadAPIVersion: "solti.io/v1", WorkloadKind: "SubprocessTask", SpecID: "spec", SpecName: "Spec", SourceResourceVersion: "opaque-rv", CapturedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	opts := CaptureOptions{MaxBytes: record.SizeBytes()}
	first, err := svc.Capture(context.Background(), captureReq(), src, opts)
	if err != nil || first.Continuation == nil || first.Continuation.Cursor != "" || first.Continuation.AfterGeneration != 3 || first.Continuation.AfterAttempt != 1 {
		t.Fatalf("intra-page continuation %+v %v", first, err)
	}
	opts.Continuation = first.Continuation
	second, err := svc.Capture(context.Background(), captureReq(), src, opts)
	if err != nil || second.Partial || second.Continuation != nil || second.Added != 1 || len(all(t, s)) != 2 {
		t.Fatalf("intra-page suffix lost %+v %v", second, err)
	}
}

func TestContinuationRejectsReplacementChangedOrExpiredSource(t *testing.T) {
	for _, kind := range []string{"replacement", "version", "expired", "missing-boundary"} {
		t.Run(kind, func(t *testing.T) {
			svc, s := newService(t, Config{})
			src := source(run(2, "failed"))
			resume := &RunContinuation{Cursor: "next", TaskUID: "uid", ResourceVersion: "opaque-rv"}
			src.pages["next"] = src.pages[""]
			switch kind {
			case "replacement":
				src.uid = "new"
				src.pages["next"].TaskUID = "new"
			case "version":
				src.pages["next"].ResourceVersion = "new-rv"
			case "expired":
				src.onPage = func(context.Context, string) (*proxy.TaskRunPage, error) { return nil, proxy.ErrRunSnapshotExpired }
			case "missing-boundary":
				resume.Cursor = ""
				resume.AfterGeneration = 3
				resume.AfterAttempt = 1
			}
			_, err := svc.Capture(context.Background(), captureReq(), src, CaptureOptions{Continuation: resume})
			if !errors.Is(err, ErrSnapshotChanged) || s.transactions != 0 || len(all(t, s)) != 0 {
				t.Fatalf("stale continuation published %v", err)
			}
		})
	}
}

func TestKnownCompleteSnapshotDoesNotRecaptureRetentionEvictions(t *testing.T) {
	svc, s := newService(t, Config{MaxRecords: 1})
	src := source(run(1, "failed"), run(2, "succeeded"))
	first, err := svc.Capture(context.Background(), captureReq(), src, CaptureOptions{})
	if err != nil || first.Added != 2 || first.Pruned != 1 {
		t.Fatalf("first retained snapshot %+v %v", first, err)
	}
	retained := all(t, s)[0]
	opts := CaptureOptions{KnownComplete: &SnapshotIdentity{TaskUID: first.TaskUID, ResourceVersion: first.SourceResourceVersion}}
	unchanged, err := svc.Capture(context.Background(), captureReq(), src, opts)
	if err != nil || !unchanged.Unchanged || unchanged.Added != 0 || s.transactions != 1 || all(t, s)[0].ID() != retained.ID() {
		t.Fatalf("unchanged pruned source rewrote archive %+v %v tx%d", unchanged, err, s.transactions)
	}
	src.pages[""].ResourceVersion = "changed-rv"
	changed, err := svc.Capture(context.Background(), captureReq(), src, opts)
	if err != nil || changed.Unchanged || changed.Added == 0 || s.transactions != 2 {
		t.Fatalf("changed version suppressed new observation %+v %v", changed, err)
	}
	src.uid = "replacement"
	src.pages[""].TaskUID = "replacement"
	replacement, err := svc.Capture(context.Background(), captureReq(), src, opts)
	if err != nil || replacement.Unchanged || replacement.Added == 0 {
		t.Fatalf("new incarnation suppressed %+v %v", replacement, err)
	}
	// Process-local optimization has no permanent tombstone: without the known
	// snapshot after restart, still-retained SDK attempts may be captured again.
	restarted, err := New(s, Config{MaxRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	again, err := restarted.Capture(context.Background(), captureReq(), src, CaptureOptions{})
	if err != nil || again.Added == 0 {
		t.Fatalf("restart recapture boundary changed %+v %v", again, err)
	}
}

// failedRow is a failed attempt of a task, captured at the given time.
func failedRow(attempt int, uid string, at time.Time) (*model.ExecutionRecord, error) {
	return model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "a", TaskName: "t", TaskUID: uid, Generation: 1, Attempt: uint32(attempt), Status: "failed", CapturedAt: at})
}

// put stores a failed attempt of a task, captured at the given time, behind the
// service: the way rows reach a store that the service did not write to.
func put(t *testing.T, s storage.Storage, attempt int, at time.Time) {
	t.Helper()
	record, err := failedRow(attempt, "u", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertExecution(context.Background(), record); err != nil {
		t.Fatal(err)
	}
}

// restart is a new service over the store of an earlier one, on the same clock.
func restart(t *testing.T, s storage.Storage, cfg Config) *Service {
	t.Helper()
	svc, err := New(s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }
	return svc
}

// attempts lists the attempt numbers of rows, ascending.
func attempts(rows []*model.ExecutionRecord) []int {
	var out []int
	for _, row := range rows {
		out = append(out, int(row.Attempt()))
	}
	slices.Sort(out)
	return out
}

// held is what svc counted.
func held(svc *Service) usage {
	_ = svc.gate.Acquire(context.Background(), 1) // a background ctx is never done
	defer svc.gate.Release(1)
	return svc.usage
}

// truth counts the rows of s afresh, without the service.
func truth(t *testing.T, s storage.Storage) usage {
	t.Helper()
	var u usage
	for _, row := range all(t, s) {
		u.rows++
		u.bytes += row.SizeBytes()
		if u.oldest.IsZero() || row.CapturedAt().Before(u.oldest) {
			u.oldest = row.CapturedAt()
		}
	}
	return u
}

func sameUsage(a, b usage) bool {
	return a.rows == b.rows && a.bytes == b.bytes && a.oldest.Equal(b.oldest)
}

func TestPruneDoesNotScanWhenWithinLimits(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t, Config{MaxRecords: 10})
	for i := range 10 {
		put(t, s, i+1, svc.now().Add(-time.Duration(i+1)*time.Hour))
	}
	// Ten rows at a limit of ten, the oldest ten hours old: nothing is due. The
	// first decision counts the history, in one pass.
	if n, err := svc.Prune(ctx); err != nil || n != 0 || s.scans.Load() != 1 {
		t.Fatalf("first prune removed %d rows in %d passes: %v", n, s.scans.Load(), err)
	}
	s.scans.Store(0)
	for range 1000 {
		if n, err := svc.Prune(ctx); err != nil || n != 0 {
			t.Fatalf("idle prune removed %d rows: %v", n, err)
		}
	}
	if scans := s.scans.Load(); scans != 0 || s.transactions != 0 {
		t.Fatalf("1000 idle prunes made %d passes over the rows and opened %d transactions", scans, s.transactions)
	}
}

func TestPruneScansOnceWhenALimitIsCrossed(t *testing.T) {
	for _, limit := range []string{"age", "count", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			ctx := context.Background()
			svc, s := newService(t, Config{})
			now := svc.now()
			for i := range 4 {
				// Attempt 1 is the oldest, captured four hours ago.
				put(t, s, i+1, now.Add(-time.Duration(4-i)*time.Hour))
			}
			size := all(t, s)[0].SizeBytes()
			// A new service counts the history first, then finds the oldest rows in a
			// second pass. One that has counted it needs the second only.
			passes := int64(2)
			switch limit {
			case "age":
				if n, err := svc.Prune(ctx); err != nil || n != 0 {
					t.Fatalf("history within its limits: removed %d: %v", n, err)
				}
				// Past the retention of the oldest row, and of no other.
				svc.now = func() time.Time { return now.Add(DefaultConfig().Retention - 3*time.Hour - 30*time.Minute) }
				passes = 1
			case "count":
				svc = restart(t, s, Config{MaxRecords: 3})
			case "bytes":
				svc = restart(t, s, Config{MaxBytes: 3 * size})
			}
			s.scans.Store(0)
			if n, err := svc.Prune(ctx); err != nil || n != 1 || s.scans.Load() != passes {
				t.Fatalf("removed %d rows in %d passes, want 1 in %d: %v", n, s.scans.Load(), passes, err)
			}
			if rows := all(t, s); !slices.Equal(attempts(rows), []int{2, 3, 4}) {
				t.Fatalf("kept attempts %v, want the newest three", attempts(rows))
			}
			if !sameUsage(held(svc), truth(t, s)) {
				t.Fatalf("counters %+v, store %+v", held(svc), truth(t, s))
			}
			s.scans.Store(0)
			opened := s.transactions
			for range 100 {
				if _, err := svc.Prune(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if s.scans.Load() != 0 || s.transactions != opened {
				t.Fatalf("pruned history was scanned or written again: %d passes, %d transactions", s.scans.Load(), s.transactions-opened)
			}
		})
	}
}

func TestCaptureScansOnlyWhenALimitIsCrossed(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t, Config{MaxRecords: 3})
	capture := func(hour int, runs ...proxyv1.TaskRun) CaptureResult {
		t.Helper()
		svc.now = func() time.Time { return time.Date(2026, 10, 2, hour, 0, 0, 0, time.UTC) }
		result, err := svc.Capture(ctx, captureReq(), source(runs...), CaptureOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	capture(1, run(1, "failed"))
	s.scans.Store(0)
	if r := capture(2, run(2, "failed"), run(3, "failed")); r.Added != 2 || r.Pruned != 0 || s.scans.Load() != 0 {
		t.Fatalf("captured within the limit: %+v, %d passes", r, s.scans.Load())
	}
	// A fourth row crosses the limit; one pass finds the oldest row to drop.
	if r := capture(3, run(4, "failed")); r.Added != 1 || r.Pruned != 1 || s.scans.Load() != 1 {
		t.Fatalf("captured over the limit: %+v, %d passes", r, s.scans.Load())
	}
	if rows := all(t, s); !slices.Equal(attempts(rows), []int{2, 3, 4}) {
		t.Fatalf("kept attempts %v, want the newest three", attempts(rows))
	}
	if !sameUsage(held(svc), truth(t, s)) {
		t.Fatalf("counters %+v, store %+v", held(svc), truth(t, s))
	}
}

func TestCaptureEnforcesEveryLimitInItsTransaction(t *testing.T) {
	for _, limit := range []string{"age", "count", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			ctx := context.Background()
			svc, s := newService(t, Config{})
			now := svc.now()
			// Two rows are archived, the older of them 90 minutes ago.
			for i, ago := range []time.Duration{90 * time.Minute, 30 * time.Minute} {
				svc.now = func() time.Time { return now.Add(-ago) }
				if _, err := svc.Capture(ctx, captureReq(), source(run(i+1, "failed")), CaptureOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			cfg := Config{}
			switch limit {
			case "age":
				cfg.Retention = time.Hour
			case "count":
				cfg.MaxRecords = 2
			case "bytes":
				cfg.MaxBytes = 2 * all(t, s)[0].SizeBytes()
			}
			svc = restart(t, s, cfg)
			result, err := svc.Capture(ctx, captureReq(), source(run(3, "failed")), CaptureOptions{})
			if err != nil || result.Added != 1 || result.Pruned != 1 {
				t.Fatalf("capture %+v: %v", result, err)
			}
			if rows := all(t, s); !slices.Equal(attempts(rows), []int{2, 3}) {
				t.Fatalf("kept attempts %v, want the newer two", attempts(rows))
			}
			if !sameUsage(held(svc), truth(t, s)) {
				t.Fatalf("counters %+v, store %+v", held(svc), truth(t, s))
			}
		})
	}
}

func TestCaptureDropsARowThatIsPastRetentionAtOnce(t *testing.T) {
	svc, s := newService(t, Config{Retention: time.Hour})
	// The row is captured when the clock reads the first time, and judged when it
	// reads the second, two hours later.
	reads := 0
	t0 := svc.now()
	svc.now = func() time.Time {
		reads++
		if reads == 1 {
			return t0
		}
		return t0.Add(2 * time.Hour)
	}
	result, err := svc.Capture(context.Background(), captureReq(), source(run(1, "failed")), CaptureOptions{})
	if err != nil || result.Added != 1 || result.Pruned != 1 || len(all(t, s)) != 0 {
		t.Fatalf("capture %+v: %v", result, err)
	}
	if got := held(svc); got.rows != 0 || got.bytes != 0 || !got.oldest.IsZero() {
		t.Fatalf("counters %+v, want none", got)
	}
}

func TestResumeReadsWhatAnotherNodeWrote(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t, Config{MaxRecords: 3})
	if n, err := svc.Prune(ctx); err != nil || n != 0 {
		t.Fatalf("empty history: removed %d: %v", n, err)
	}
	// Another node leads for a while and writes five rows, one more hour apart.
	for i := range 5 {
		put(t, s, i+1, svc.now().Add(-time.Duration(5-i)*time.Hour))
	}
	s.scans.Store(0)
	// Nothing has told the service: what it counted is all it knows.
	if n, err := svc.Prune(ctx); err != nil || n != 0 || s.scans.Load() != 0 {
		t.Fatalf("removed %d rows in %d passes before it was told of the writes: %v", n, s.scans.Load(), err)
	}
	svc.Resume()
	// Told, it counts the rows again before it decides, and finds the oldest two.
	if n, err := svc.Prune(ctx); err != nil || n != 2 || s.scans.Load() != 2 {
		t.Fatalf("removed %d rows in %d passes after the writes, want 2 in 2: %v", n, s.scans.Load(), err)
	}
	if rows := all(t, s); !slices.Equal(attempts(rows), []int{3, 4, 5}) {
		t.Fatalf("kept attempts %v, want the newest three", attempts(rows))
	}
	if !sameUsage(held(svc), truth(t, s)) {
		t.Fatalf("counters %+v, store %+v", held(svc), truth(t, s))
	}
	s.scans.Store(0)
	if _, err := svc.Prune(ctx); err != nil || s.scans.Load() != 0 {
		t.Fatalf("a second prune made %d passes: %v", s.scans.Load(), err)
	}
}

func TestFailedTransactionMarksCountersStale(t *testing.T) {
	failure := errors.New("commit failed")
	cases := []struct {
		name    string
		inject  func(*countedStore) // how the next transaction fails
		capture bool                // a capture runs it, a prune otherwise
		due     bool                // both rows are past retention by then
		removed int                 // what the next prune removes
		scans   int64               // the passes it makes over the rows
		opened  int                 // the transactions it opens
	}{
		{name: "capture fails before it runs", inject: func(s *countedStore) { s.failure = failure }, capture: true, scans: 1},
		{name: "capture commits, then fails", inject: func(s *countedStore) { s.landed = failure }, capture: true, scans: 1},
		{name: "prune fails before it runs", inject: func(s *countedStore) { s.failure = failure }, due: true, removed: 2, scans: 2, opened: 1},
		{name: "prune commits, then fails", inject: func(s *countedStore) { s.landed = failure }, due: true, scans: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, s := newService(t, Config{Retention: 24 * time.Hour})
			now := svc.now()
			put(t, s, 1, now.Add(-time.Hour))
			put(t, s, 2, now.Add(-2*time.Hour))
			if _, err := svc.Prune(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.due {
				svc.now = func() time.Time { return now.Add(25 * time.Hour) }
			}
			tc.inject(s)
			var err error
			if tc.capture {
				_, err = svc.Capture(ctx, captureReq(), source(run(3, "failed")), CaptureOptions{})
			} else {
				_, err = svc.Prune(ctx)
			}
			if !errors.Is(err, failure) {
				t.Fatalf("the failure was not reported: %v", err)
			}
			// Whether the write landed or not, the service cannot tell: it counts again.
			s.failure, s.landed = nil, nil
			s.scans.Store(0)
			before := s.transactions
			if n, err := svc.Prune(ctx); err != nil || n != tc.removed || s.scans.Load() != tc.scans || s.transactions-before != tc.opened {
				t.Fatalf("next prune removed %d rows in %d passes and %d transactions, want %d in %d and %d: %v",
					n, s.scans.Load(), s.transactions-before, tc.removed, tc.scans, tc.opened, err)
			}
			if !sameUsage(held(svc), truth(t, s)) {
				t.Fatalf("counters %+v, store %+v", held(svc), truth(t, s))
			}
		})
	}
}

func TestFailedCountLeavesCountersStale(t *testing.T) {
	ctx := context.Background()
	s := &unreadable{Storage: inmemory.New(), err: errors.New("listing failed")}
	svc := restart(t, s, Config{})
	put(t, s, 1, svc.now().Add(-time.Hour))
	if _, err := svc.Prune(ctx); !errors.Is(err, s.err) {
		t.Fatalf("counted a history that could not be read: %v", err)
	}
	s.err = nil
	if n, err := svc.Prune(ctx); err != nil || n != 0 || held(svc).rows != 1 {
		t.Fatalf("prune after the failed count removed %d rows, counted %d: %v", n, held(svc).rows, err)
	}
}

func TestDeadlineEndsTheWaitForTheGate(t *testing.T) {
	cases := []struct {
		name string
		call func(context.Context, *Service) error
	}{
		{"prune", func(ctx context.Context, svc *Service) error {
			_, err := svc.Prune(ctx)
			return err
		}},
		{"capture", func(ctx context.Context, svc *Service) error {
			_, err := svc.Capture(ctx, captureReq(), source(run(3, "failed")), CaptureOptions{})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, s := newService(t, Config{})
			put(t, s, 1, svc.now().Add(-time.Hour))
			put(t, s, 2, svc.now().Add(-2*time.Hour))
			if _, err := svc.Prune(ctx); err != nil {
				t.Fatal(err)
			}
			counted, stored := held(svc), kept(all(t, s))

			// A commit that never ends holds the gate.
			if err := svc.gate.Acquire(ctx, 1); err != nil {
				t.Fatal(err)
			}
			holding := true
			release := func() {
				if holding {
					holding = false
					svc.gate.Release(1)
				}
			}
			defer release()
			short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			scans, opened := s.scans.Load(), s.transactions
			done := make(chan error, 1)
			go func() { done <- tc.call(short, svc) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("returned %v, want its deadline", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("kept waiting for the gate long after its deadline")
			}
			release()

			// A wait that was cut short changed nothing, so the counters are still exact.
			if s.scans.Load() != scans || s.transactions != opened {
				t.Fatalf("the call read %d passes and opened %d transactions", s.scans.Load()-scans, s.transactions-opened)
			}
			if got := held(svc); !sameUsage(got, counted) {
				t.Fatalf("counters %+v, were %+v", got, counted)
			}
			if got := kept(all(t, s)); !slices.Equal(got, stored) {
				t.Fatalf("store holds %v, held %v", got, stored)
			}
			s.scans.Store(0)
			if n, err := svc.Prune(ctx); err != nil || n != 0 || s.scans.Load() != 0 {
				t.Fatalf("prune after the wait removed %d rows in %d passes: %v", n, s.scans.Load(), err)
			}
		})
	}
}

// kept names rows by attempt, capture time and ID, in a fixed order.
func kept(rows []*model.ExecutionRecord) []string {
	var out []string
	for _, row := range rows {
		out = append(out, fmt.Sprintf("%d@%s/%s", row.Attempt(), row.CapturedAt().UTC().Format(time.RFC3339), row.ID()))
	}
	slices.Sort(out)
	return out
}

// captured is the record that capturing run(attempt, "failed") makes at the given
// time. The test builds it from the fixtures of the capture, to play the service.
func captured(t *testing.T, attempt int, at time.Time) *model.ExecutionRecord {
	t.Helper()
	req, r, src := captureReq(), run(attempt, "failed"), source(run(attempt, "failed"))
	record, err := model.NewExecutionRecord(model.ExecutionRecordParams{
		AgentID: req.AgentID, AgentName: req.AgentName, TaskName: req.TaskName, TaskUID: src.uid,
		Generation: r.Generation, Attempt: uint32(r.Attempt), Status: r.Status,
		StartedAtMs: r.StartedAt, FinishedAtMs: r.FinishedAt,
		WorkloadAPIVersion: r.WorkloadAPIVersion, WorkloadKind: r.WorkloadKind,
		SpecID: req.SpecID, SpecName: req.SpecName, SourceResourceVersion: src.pages[""].ResourceVersion, CapturedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// retained is what retention keeps of rows at now, by the loop the service ran
// over every row before it kept counters: the oldest row goes while it is past
// retention or a limit is crossed, and of rows captured at one time the lesser ID
// goes first.
func retained(cfg Config, rows []*model.ExecutionRecord, now time.Time) []*model.ExecutionRecord {
	sorted := append([]*model.ExecutionRecord(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].CapturedAt().Equal(sorted[j].CapturedAt()) {
			return sorted[i].ID() < sorted[j].ID()
		}
		return sorted[i].CapturedAt().Before(sorted[j].CapturedAt())
	})
	var bytes int64
	for _, record := range sorted {
		bytes += record.SizeBytes()
	}
	expiredBefore := now.Add(-cfg.Retention)
	for len(sorted) > 0 {
		oldest := sorted[0]
		if !oldest.CapturedAt().Before(expiredBefore) && len(sorted) <= cfg.MaxRecords && bytes <= cfg.MaxBytes {
			break
		}
		sorted = sorted[1:]
		bytes -= oldest.SizeBytes()
	}
	return sorted
}

func TestRetentionHoldsAndCountersStayExactAcrossRandomHistories(t *testing.T) {
	ctx := context.Background()
	probe, probed := newService(t, Config{})
	if _, err := probe.Capture(ctx, captureReq(), source(run(1, "failed")), CaptureOptions{}); err != nil {
		t.Fatal(err)
	}
	size := all(t, probed)[0].SizeBytes()
	for seed := range uint64(50) {
		rng := rand.New(rand.NewPCG(seed, 0))
		cfg := Config{
			Retention:  time.Duration(1+rng.IntN(6)) * time.Hour,
			MaxRecords: 1 + rng.IntN(8),
			MaxBytes:   int64(1+rng.IntN(8)) * size,
		}
		svc, s := newService(t, cfg)
		clock := svc.now()
		svc.now = func() time.Time { return clock }
		// What the archive should hold, by the reference loop over the test's own records.
		var reference []*model.ExecutionRecord
		for step := range 30 {
			// The clock mostly moves on; now and then it goes back, as a skewed one does.
			clock = clock.Add(time.Duration(rng.IntN(5)-1) * 30 * time.Minute)
			enforced := true
			if rng.IntN(3) == 0 {
				after := retained(cfg, reference, clock)
				n, err := svc.Prune(ctx)
				if err != nil || n != len(reference)-len(after) {
					t.Fatalf("seed %d step %d: prune removed %d rows, reference %d: %v", seed, step, n, len(reference)-len(after), err)
				}
				reference = after
			} else {
				picked := map[int]bool{}
				for range 1 + rng.IntN(4) {
					picked[1+rng.IntN(40)] = true
				}
				// One capture stamps all the rows it adds with one time, so they tie.
				var runs []proxyv1.TaskRun
				var incoming []*model.ExecutionRecord
				for _, attempt := range slices.Sorted(maps.Keys(picked)) {
					runs = append(runs, run(attempt, "failed"))
					record := captured(t, attempt, clock)
					if !slices.ContainsFunc(reference, func(r *model.ExecutionRecord) bool { return r.ID() == record.ID() }) {
						incoming = append(incoming, record)
					}
				}
				result, err := svc.Capture(ctx, captureReq(), source(runs...), CaptureOptions{})
				if err != nil {
					t.Fatalf("seed %d step %d: %v", seed, step, err)
				}
				pruned := 0
				if len(incoming) > 0 {
					after := retained(cfg, append(slices.Clone(reference), incoming...), clock)
					pruned = len(reference) + len(incoming) - len(after)
					reference = after
				}
				// A capture of rows all archived already writes nothing, so judges nothing.
				enforced = len(incoming) > 0
				if result.Added != len(incoming) || result.Existing != len(runs)-len(incoming) || result.Pruned != pruned {
					t.Fatalf("seed %d step %d: capture %+v, reference added %d existing %d pruned %d", seed, step, result, len(incoming), len(runs)-len(incoming), pruned)
				}
			}
			rows := all(t, s)
			if !slices.Equal(kept(rows), kept(reference)) {
				t.Fatalf("seed %d step %d: store holds attempts %v, reference %v", seed, step, attempts(rows), attempts(reference))
			}
			if !sameUsage(held(svc), truth(t, s)) {
				t.Fatalf("seed %d step %d: counters %+v, store %+v", seed, step, held(svc), truth(t, s))
			}
			if !enforced {
				continue
			}
			got := truth(t, s)
			if got.rows > cfg.MaxRecords || got.bytes > cfg.MaxBytes || (got.rows > 0 && got.oldest.Before(clock.Add(-cfg.Retention))) {
				t.Fatalf("seed %d step %d: %d rows, %d bytes, oldest %v kept at %v under %+v", seed, step, len(rows), got.bytes, got.oldest, clock, cfg)
			}
		}
	}
}

func TestConcurrentCommitsKeepCountersExact(t *testing.T) {
	ctx := context.Background()
	const writers, each, limit = 8, 25, 40
	svc, s := newService(t, Config{MaxRecords: limit})
	base := svc.now()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for j := range each {
				n := w*each + j
				// Attempt n+1 is captured n seconds before attempt writers*each.
				record, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "a", TaskName: "t", TaskUID: "u", Generation: 1, Attempt: uint32(n + 1), Status: "failed", CapturedAt: base.Add(-time.Duration(writers*each-n) * time.Second)})
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := svc.commit(ctx, []*model.ExecutionRecord{record}, CaptureResult{}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	rows := all(t, s)
	got := attempts(rows)
	if len(got) != limit || got[0] != writers*each-limit+1 {
		t.Fatalf("kept %d rows from attempt %v, want the newest %d", len(got), got[:1], limit)
	}
	if !sameUsage(held(svc), truth(t, s)) {
		t.Fatalf("counters %+v, store %+v", held(svc), truth(t, s))
	}
}

func TestResumeRacingDecisionsKeepsCountersExact(t *testing.T) {
	ctx := context.Background()
	const writers, each, foreign = 4, 25, 50
	svc, s := newService(t, Config{MaxRecords: 1000})
	base := svc.now()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			<-start
			for j := range each {
				n := w*each + j
				record, err := failedRow(n+1, "u", base.Add(-time.Duration(n)*time.Second))
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := svc.commit(ctx, []*model.ExecutionRecord{record}, CaptureResult{}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for range 2 {
		wg.Go(func() {
			<-start
			for range 100 {
				if _, err := svc.Prune(ctx); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	// Another node writes, and each of its writes is followed by the Resume that
	// reports it. Other Resumes report nothing.
	wg.Go(func() {
		<-start
		for j := range foreign {
			record, err := failedRow(j+1, "foreign", base.Add(-time.Duration(j)*time.Second))
			if err != nil {
				t.Error(err)
				return
			}
			if err := s.UpsertExecution(ctx, record); err != nil {
				t.Error(err)
				return
			}
			svc.Resume()
		}
	})
	for range 2 {
		wg.Go(func() {
			<-start
			for range 200 {
				svc.Resume()
			}
		})
	}
	close(start)
	wg.Wait()
	// One more decision, with no Resume of its own, settles what the others left
	// pending: every write was reported after it was made, so none is missed.
	if _, err := svc.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	want := truth(t, s)
	if want.rows != writers*each+foreign {
		t.Fatalf("store holds %d rows, want %d", want.rows, writers*each+foreign)
	}
	if got := held(svc); !sameUsage(got, want) {
		t.Fatalf("counters %+v, store %+v", got, want)
	}
}
