package history

import (
	"context"
	"errors"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"slices"
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
	"github.com/soltiHQ/control-plane/internal/storage/durable"
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
	s.failure = errors.New("durable fsync failed")
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

// put stores a failed attempt of a task, captured at the given time, behind the
// service: the way rows reach a store that the service did not write to.
func put(t *testing.T, s storage.Storage, attempt int, at time.Time) {
	t.Helper()
	record, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "a", TaskName: "t", TaskUID: "u", Generation: 1, Attempt: uint32(attempt), Status: "failed", CapturedAt: at})
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
	svc.mu.Lock()
	defer svc.mu.Unlock()
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

func TestCountersMatchStoreAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	open := func() *durable.Store {
		t.Helper()
		store, err := durable.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	cfg := Config{Retention: 12 * time.Hour, MaxRecords: 5}
	epoch := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	first := open()
	svc, err := New(first, cfg)
	if err != nil {
		t.Fatal(err)
	}
	capture := func(hour int) {
		t.Helper()
		svc.now = func() time.Time { return epoch.Add(time.Duration(hour) * time.Hour) }
		if _, err := svc.Capture(ctx, captureReq(), source(run(hour+1, "failed")), CaptureOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Ten captures an hour apart: the count limit keeps the newest five. One more,
	// twenty hours in, ages out all but the last two of those.
	for hour := range 10 {
		capture(hour)
	}
	capture(20)
	want := truth(t, first)
	if want.rows != 3 {
		t.Fatalf("history of %d rows, want the 3 that retention leaves", want.rows)
	}
	if got := held(svc); !sameUsage(got, want) {
		t.Fatalf("counters %+v, store %+v", got, want)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := open()
	scans := new(atomic.Int64)
	restarted, err := New(counting{Storage: second, scans: scans}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = func() time.Time { return epoch.Add(20 * time.Hour) }
	if n, err := restarted.Prune(ctx); err != nil || n != 0 || scans.Load() != 1 {
		t.Fatalf("first prune after the restart removed %d rows in %d passes: %v", n, scans.Load(), err)
	}
	if got := held(restarted); !sameUsage(got, want) {
		t.Fatalf("counters after the restart %+v, store %+v", got, want)
	}
	for range 100 {
		if _, err := restarted.Prune(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if scans.Load() != 1 {
		t.Fatalf("pruning a counted history made %d passes", scans.Load()-1)
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
	svc, s := newService(t, Config{})
	put(t, s, 1, svc.now().Add(-time.Hour))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.Prune(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("counted the history with a canceled context: %v", err)
	}
	if n, err := svc.Prune(ctx); err != nil || n != 0 || held(svc).rows != 1 {
		t.Fatalf("prune after the failed count removed %d rows, counted %d: %v", n, held(svc).rows, err)
	}
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
		for step := range 30 {
			// The clock mostly moves on; now and then it goes back, as a skewed one does.
			clock = clock.Add(time.Duration(rng.IntN(5)-1) * 30 * time.Minute)
			enforced := true
			var err error
			if rng.IntN(3) == 0 {
				_, err = svc.Prune(ctx)
			} else {
				picked := map[int]bool{}
				for range 1 + rng.IntN(4) {
					picked[1+rng.IntN(40)] = true
				}
				var runs []proxyv1.TaskRun
				for _, attempt := range slices.Sorted(maps.Keys(picked)) {
					runs = append(runs, run(attempt, "failed"))
				}
				var result CaptureResult
				result, err = svc.Capture(ctx, captureReq(), source(runs...), CaptureOptions{})
				// A capture of rows all archived already writes, so judges, nothing.
				enforced = result.Added > 0
			}
			if err != nil {
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
			rows := all(t, s)
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
