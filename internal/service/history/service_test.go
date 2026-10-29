package history

import (
	"context"
	"errors"
	"strings"
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

type countedStore struct {
	storage.Storage
	transactions int
	failure      error
}

func (s *countedStore) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	s.transactions++
	if s.failure != nil {
		return s.failure
	}
	return s.Storage.WithTx(ctx, fn)
}

func newService(t *testing.T, cfg Config) (*Service, *countedStore) {
	t.Helper()
	store := &countedStore{Storage: inmemory.New()}
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
