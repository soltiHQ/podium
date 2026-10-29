// Package history archives observed terminal attempts without depending on the
// live agent's continued existence. Collection is best effort: source retention,
// unavailable agents and deletion before observation can leave permanent gaps.
package history

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage"
)

type Service struct {
	store storage.Storage
	cfg   Config
	now   func() time.Time
}

func New(store storage.Storage, cfg Config) (*Service, error) {
	if store == nil {
		return nil, storage.ErrInvalidArgument
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Service{store: store, cfg: cfg.WithDefaults(), now: func() time.Time { return time.Now().UTC() }}, nil
}
func (s *Service) Config() Config { return s.cfg }
func (s *Service) List(ctx context.Context, q ListQuery) (*storage.ExecutionListResult, error) {
	for _, field := range []string{q.Criteria.AgentID, q.Criteria.SpecID, q.Criteria.TaskName, q.Criteria.TaskUID} {
		if len(field) > model.ExecutionMetadataMaxBytes || !utf8.ValidString(field) {
			return nil, storage.ErrInvalidArgument
		}
	}
	for _, status := range q.Criteria.Statuses {
		if !model.IsTerminalExecutionStatus(status) {
			return nil, storage.ErrInvalidArgument
		}
	}
	return s.store.ListExecutions(ctx, s.store.BuildExecutionFilter(q.Criteria), storage.ListOptions{Limit: q.Limit, Cursor: q.Cursor})
}

func (s *Service) Get(ctx context.Context, id string) (*model.ExecutionRecord, error) {
	return s.store.GetExecution(ctx, id)
}

// Capture reads one bounded, UID- and resourceVersion-consistent run snapshot.
// Source calls happen before the transaction; cancellation or malformed/mixed
// pages therefore cannot publish a partially validated batch. A deliberate page
// or byte bound records an honest observed prefix with Partial=true.
var ErrSnapshotChanged = errors.New("history: run snapshot changed or expired")

func (s *Service) Capture(ctx context.Context, req CaptureRequest, source proxy.AgentProxy, opts CaptureOptions) (CaptureResult, error) {
	var result CaptureResult
	if source == nil || req.AgentID == "" || req.TaskName == "" {
		return result, storage.ErrInvalidArgument
	}
	for _, field := range []string{req.AgentID, req.AgentName, req.TaskName, req.SpecID, req.SpecName} {
		if len(field) > model.ExecutionMetadataMaxBytes || !utf8.ValidString(field) {
			return result, storage.ErrInvalidArgument
		}
	}
	opts = opts.withDefaults()
	if opts.PageSize > 1000 || opts.MaxPages < 1 || opts.MaxPages > 64 || opts.MaxBytes < 1 {
		return result, storage.ErrInvalidArgument
	}
	cursor := ""
	expectedRV := ""
	var afterGeneration uint64
	var afterAttempt uint32
	if known := opts.KnownComplete; known != nil && (known.TaskUID == "" || known.ResourceVersion == "" || len(known.TaskUID) > model.ExecutionMetadataMaxBytes || len(known.ResourceVersion) > model.ExecutionMetadataMaxBytes || !utf8.ValidString(known.TaskUID) || !utf8.ValidString(known.ResourceVersion)) {
		return result, storage.ErrInvalidArgument
	}
	if resume := opts.Continuation; resume != nil {
		if resume.TaskUID == "" || resume.ResourceVersion == "" || len(resume.TaskUID) > model.ExecutionMetadataMaxBytes || len(resume.ResourceVersion) > model.ExecutionMetadataMaxBytes || !utf8.ValidString(resume.TaskUID) || !utf8.ValidString(resume.ResourceVersion) || len(resume.Cursor) > 8192 || (resume.AfterGeneration == 0) != (resume.AfterAttempt == 0) {
			return result, storage.ErrInvalidArgument
		}
		cursor = resume.Cursor
		expectedRV = resume.ResourceVersion
		afterGeneration = resume.AfterGeneration
		afterAttempt = resume.AfterAttempt
	}
	pager, ok := source.(proxy.TaskRunPager)
	if !ok {
		return result, fmt.Errorf("history: agent transport has no paginated run contract")
	}
	task, err := source.GetTask(ctx, req.TaskName)
	if err != nil {
		return result, err
	}
	if task == nil || task.Task == nil || task.Task.UID == "" || task.Task.ID != req.TaskName {
		return result, fmt.Errorf("history: invalid task identity")
	}
	if len(task.Task.UID) > model.ExecutionMetadataMaxBytes || !utf8.ValidString(task.Task.UID) {
		return result, fmt.Errorf("history: invalid or oversized task UID")
	}
	uid := task.Task.UID
	result.TaskUID = uid
	if opts.Continuation != nil && opts.Continuation.TaskUID != uid {
		return result, ErrSnapshotChanged
	}
	capturedAt := s.now()
	seen := map[string]bool{cursor: true}
	candidates := map[string]*model.ExecutionRecord{}
	var bytes int64
	var previousGeneration uint64
	var previousAttempt uint32
	complete := false
	for pageNumber := 0; pageNumber < opts.MaxPages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		page, err := pager.ListTaskRunsPage(ctx, req.TaskName, proxy.TaskRunQuery{Limit: opts.PageSize, Cursor: cursor})
		if err != nil {
			if errors.Is(err, proxy.ErrRunSnapshotExpired) {
				return result, fmt.Errorf("%w: %w", ErrSnapshotChanged, err)
			}
			return result, err
		}
		if page == nil || page.TaskUID != uid || page.ResourceVersion == "" {
			return result, fmt.Errorf("%w: run snapshot identity changed or missing", ErrSnapshotChanged)
		}
		if len(page.ResourceVersion) > model.ExecutionMetadataMaxBytes || !utf8.ValidString(page.ResourceVersion) {
			return result, fmt.Errorf("history: invalid or oversized resource version")
		}
		if expectedRV != "" && expectedRV != page.ResourceVersion {
			return result, fmt.Errorf("%w: resource version changed", ErrSnapshotChanged)
		}
		expectedRV = page.ResourceVersion
		result.SourceResourceVersion = page.ResourceVersion
		if pageNumber == 0 && opts.Continuation == nil && opts.KnownComplete != nil && opts.KnownComplete.TaskUID == uid && opts.KnownComplete.ResourceVersion == page.ResourceVersion {
			// Source UID/version fixes the previously completed collection. Preserve
			// retention evictions instead of recapturing the same unchanged attempts.
			result.Unchanged = true
			return result, ctx.Err()
		}
		if len(page.NextCursor) > 8192 {
			return result, fmt.Errorf("history: oversized source cursor")
		}
		if len(page.Runs) > int(opts.PageSize) {
			return result, fmt.Errorf("history: source exceeded requested page size")
		}
		lastGeneration, lastAttempt := afterGeneration, afterAttempt
		boundaryFound := afterGeneration == 0
		for _, run := range page.Runs {
			if run.Generation == 0 || run.Attempt <= 0 || uint64(run.Attempt) > uint64(^uint32(0)) {
				return result, fmt.Errorf("history: invalid run identity")
			}
			attempt := uint32(run.Attempt)
			if previousGeneration > run.Generation || (previousGeneration == run.Generation && previousAttempt >= attempt) {
				return result, fmt.Errorf("history: source run identities are not strictly ordered")
			}
			previousGeneration, previousAttempt = run.Generation, attempt
			if afterGeneration != 0 && (run.Generation < afterGeneration || (run.Generation == afterGeneration && attempt <= afterAttempt)) {
				if run.Generation == afterGeneration && attempt == afterAttempt {
					boundaryFound = true
				}
				continue
			}
			if !boundaryFound {
				return result, fmt.Errorf("%w: intra-page boundary missing", ErrSnapshotChanged)
			}
			if !model.IsTerminalExecutionStatus(run.Status) {
				if run.Status == "pending" || run.Status == "running" {
					lastGeneration, lastAttempt = run.Generation, attempt
					continue
				}
				return result, fmt.Errorf("history: unknown source run status %q", run.Status)
			}
			if !run.FinishedAtPresent {
				return result, fmt.Errorf("history: terminal attempt has no finish timestamp")
			}
			record, err := model.NewExecutionRecord(model.ExecutionRecordParams{
				AgentID: req.AgentID, AgentName: req.AgentName, TaskName: req.TaskName, TaskUID: uid,
				Generation: run.Generation, Attempt: attempt, Status: run.Status,
				StartedAtMs: run.StartedAt, FinishedAtMs: run.FinishedAt, ExitCode: run.ExitCode, Error: run.Error,
				WorkloadAPIVersion: run.WorkloadAPIVersion, WorkloadKind: run.WorkloadKind,
				SpecID: req.SpecID, SpecName: req.SpecName, SourceResourceVersion: page.ResourceVersion, CapturedAt: capturedAt,
			})
			if err != nil {
				return result, fmt.Errorf("history: invalid source run: %w", err)
			}
			if record.SizeBytes() > opts.MaxBytes-bytes {
				if len(candidates) == 0 {
					return result, fmt.Errorf("history: capture byte budget cannot hold one source record")
				}
				result.Partial = true
				complete = true
				result.Continuation = &RunContinuation{Cursor: cursor, TaskUID: uid, ResourceVersion: expectedRV, AfterGeneration: lastGeneration, AfterAttempt: lastAttempt}
				break
			}
			candidates[record.ID()] = record
			bytes += record.SizeBytes()
			result.Observed++
			lastGeneration, lastAttempt = run.Generation, attempt
		}
		if !boundaryFound {
			return result, fmt.Errorf("%w: intra-page boundary absent", ErrSnapshotChanged)
		}
		if complete {
			break
		}
		cursor = page.NextCursor
		afterGeneration, afterAttempt = 0, 0
		if cursor == "" {
			complete = true
			break
		}
		if seen[cursor] {
			return result, fmt.Errorf("history: repeated source cursor")
		}
		seen[cursor] = true
	}
	if !complete {
		result.Partial = true
		result.Continuation = &RunContinuation{Cursor: cursor, TaskUID: uid, ResourceVersion: expectedRV}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	records := make([]*model.ExecutionRecord, 0, len(candidates))
	for _, record := range candidates {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID() < records[j].ID() })
	return s.commit(ctx, records, result)
}

func (s *Service) commit(ctx context.Context, records []*model.ExecutionRecord, result CaptureResult) (CaptureResult, error) {
	pending := make([]*model.ExecutionRecord, 0, len(records))
	for _, record := range records {
		_, err := s.store.GetExecution(ctx, record.ID())
		if err == nil {
			result.Existing++
			continue
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return result, err
		}
		pending = append(pending, record)
	}
	if len(pending) == 0 {
		return result, ctx.Err()
	}
	added, pruned := 0, 0
	now := s.now()
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		for _, record := range pending {
			if _, err := tx.GetExecution(ctx, record.ID()); err == nil {
				continue
			} else if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			if err := tx.UpsertExecution(ctx, record); err != nil {
				return err
			}
			added++
		}
		rows, err := storage.AllExecutions(ctx, tx, tx.BuildExecutionFilter(storage.ExecutionQueryCriteria{}))
		if err != nil {
			return err
		}
		for _, record := range s.toPrune(rows, now) {
			if err := tx.DeleteExecution(ctx, record.ID()); err != nil {
				return err
			}
			pruned++
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Added = added
	result.Pruned = pruned
	return result, nil
}

func (s *Service) toPrune(rows []*model.ExecutionRecord, now time.Time) []*model.ExecutionRecord {
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
	count := len(sorted)
	expiredBefore := now.Add(-s.cfg.Retention)
	var remove []*model.ExecutionRecord
	for _, record := range sorted {
		if !record.CapturedAt().Before(expiredBefore) && count <= s.cfg.MaxRecords && bytes <= s.cfg.MaxBytes {
			break
		}
		remove = append(remove, record)
		count--
		bytes -= record.SizeBytes()
	}
	return remove
}

// Prune skips the write transaction when retained history already fits, so an
// idle collector does not repeatedly fsync the standalone full-state snapshot.
func (s *Service) Prune(ctx context.Context) (int, error) {
	rows, err := storage.AllExecutions(ctx, s.store, s.store.BuildExecutionFilter(storage.ExecutionQueryCriteria{}))
	if err != nil {
		return 0, err
	}
	now := s.now()
	if len(s.toPrune(rows, now)) == 0 {
		return 0, ctx.Err()
	}
	removed := 0
	err = s.store.WithTx(ctx, func(tx storage.Storage) error {
		current, err := storage.AllExecutions(ctx, tx, tx.BuildExecutionFilter(storage.ExecutionQueryCriteria{}))
		if err != nil {
			return err
		}
		for _, record := range s.toPrune(current, now) {
			if err := tx.DeleteExecution(ctx, record.ID()); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}
