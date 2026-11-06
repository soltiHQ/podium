// Package history archives observed terminal attempts without depending on the
// live agent's continued existence. Collection is best effort: source retention,
// unavailable agents and deletion before observation can leave permanent gaps.
package history

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage"
	"golang.org/x/sync/semaphore"
)

// usage is what the archive holds, as retention weighs it against its limits.
type usage struct {
	rows   int
	bytes  int64     // the sum of ExecutionRecord.SizeBytes
	oldest time.Time // when the oldest row was captured; zero while there are none
}

// add counts one more row.
func (u *usage) add(e *model.ExecutionRecord) {
	if at := e.CapturedAt(); u.rows == 0 || at.Before(u.oldest) {
		u.oldest = at
	}
	u.rows++
	u.bytes += e.SizeBytes()
}

// measure counts rows.
func measure(rows []*model.ExecutionRecord) usage {
	var u usage
	for _, e := range rows {
		u.add(e)
	}
	return u
}

// exceeds reports whether retention drops a row at now: a limit of cfg is crossed
// or the oldest row has aged out.
func (u usage) exceeds(cfg Config, now time.Time) bool {
	return u.rows > cfg.MaxRecords || u.bytes > cfg.MaxBytes || (u.rows > 0 && u.oldest.Before(now.Add(-cfg.Retention)))
}

// Service archives terminal attempts and enforces the retention of the archive.
//
// Retention weighs the archive by counters instead of reading it: a pass over the
// rows happens only when a limit is crossed. A counter that is off would leave a
// limit unenforced, so the service skips a pass only while the counters are
// exact: one pass counts the store, and from then on only the transactions of this
// service that committed change it. They stop being exact when a transaction of
// the service fails, as it may still have landed, and when Resume reports that
// another node may have written. The next decision counts again.
//
// There must be one Service per store. The counters know of the writes of their
// own Service only, so a second one over the same store would under-count
// silently.
type Service struct {
	store storage.Storage
	cfg   Config
	now   func() time.Time

	// gate orders each retention decision and each write with the counters they
	// read and move. A write holds it across its transaction, as the stores run one
	// transaction at a time anyway. A caller waits for it as long as its ctx lasts,
	// as for the writer gate of the durable store, so a capture on a deadline gives
	// up on a stuck commit and changes nothing.
	gate  *semaphore.Weighted
	usage usage
	exact bool // usage is what the store holds
	// resumed is set by Resume, which must not wait for a write.
	resumed atomic.Bool
}

// Resume tells the service that this node resumes history work after a pause in
// which another node may have written to the archive. The runners call it when
// they take leadership. Only the node that leads writes, and nothing else tells
// this one what the others did, so the next decision counts the store again.
func (s *Service) Resume() { s.resumed.Store(true) }

// New returns the Service that archives into store. There must be one per store:
// see Service.
func New(store storage.Storage, cfg Config) (*Service, error) {
	if store == nil {
		return nil, storage.ErrInvalidArgument
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Service{store: store, cfg: cfg.WithDefaults(), now: func() time.Time { return time.Now().UTC() }, gate: semaphore.NewWeighted(1)}, nil
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
				// The status is the agent's to choose: the runner logs this error,
				// so it holds at most the cut the proxy makes of an agent's text.
				return result, fmt.Errorf("history: unknown source run status %s", proxy.CapAgentText(strconv.Quote(run.Status)))
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
	if err := s.gate.Acquire(ctx, 1); err != nil {
		return result, err
	}
	defer s.gate.Release(1)
	if err := s.seed(ctx); err != nil {
		return result, err
	}
	var added, pruned int
	var after usage
	now := s.now()
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		added, pruned, after = 0, 0, s.usage
		for _, record := range pending {
			if _, err := tx.GetExecution(ctx, record.ID()); err == nil {
				continue
			} else if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			if err := tx.UpsertExecution(ctx, record); err != nil {
				return err
			}
			after.add(record)
			added++
		}
		if !after.exceeds(s.cfg, now) {
			return nil
		}
		var err error
		pruned, after, err = s.evict(ctx, tx, now)
		return err
	})
	if err != nil {
		// The write may have landed whatever the error says, as when a deadline
		// passes while the store commits.
		s.exact = false
		return result, err
	}
	s.usage = after
	result.Added = added
	result.Pruned = pruned
	return result, nil
}

// allRows reads every row of st in one pass, newest first.
func allRows(ctx context.Context, st storage.Storage) ([]*model.ExecutionRecord, error) {
	return storage.AllExecutions(ctx, st, st.BuildExecutionFilter(storage.ExecutionQueryCriteria{}))
}

// seed makes the counters exact, with one pass over the store, if they are not.
// It expects the gate held.
func (s *Service) seed(ctx context.Context) error {
	if s.resumed.Swap(false) {
		s.exact = false
	}
	if s.exact {
		return nil
	}
	rows, err := allRows(ctx, s.store)
	if err != nil {
		return err
	}
	s.usage, s.exact = measure(rows), true
	return nil
}

// evict deletes in tx the rows that retention drops at now, and returns how many
// and what is left of the archive. Rows leave in the order of their capture, the
// oldest first, so it takes one pass over the whole archive, sorted that way.
func (s *Service) evict(ctx context.Context, tx storage.Storage, now time.Time) (int, usage, error) {
	rows, err := allRows(ctx, tx)
	if err != nil {
		return 0, usage{}, err
	}
	slices.SortFunc(rows, func(a, b *model.ExecutionRecord) int {
		if c := a.CapturedAt().Compare(b.CapturedAt()); c != 0 {
			return c
		}
		return strings.Compare(a.ID(), b.ID())
	})
	left := measure(rows)
	expiredBefore := now.Add(-s.cfg.Retention)
	removed := 0
	for _, record := range rows {
		if !record.CapturedAt().Before(expiredBefore) && left.rows <= s.cfg.MaxRecords && left.bytes <= s.cfg.MaxBytes {
			break
		}
		if err := tx.DeleteExecution(ctx, record.ID()); err != nil {
			return 0, usage{}, err
		}
		removed++
		left.rows--
		left.bytes -= record.SizeBytes()
	}
	left.oldest = time.Time{}
	if removed < len(rows) {
		left.oldest = rows[removed].CapturedAt()
	}
	return removed, left, nil
}

// Prune enforces retention. It consults the counters and opens a transaction,
// which scans the archive, only when a limit is crossed: the age of the oldest
// row, the rows or the bytes. An idle collector therefore neither scans the store
// nor writes to it every tick.
func (s *Service) Prune(ctx context.Context) (int, error) {
	if err := s.gate.Acquire(ctx, 1); err != nil {
		return 0, err
	}
	defer s.gate.Release(1)
	if err := s.seed(ctx); err != nil {
		return 0, err
	}
	now := s.now()
	if !s.usage.exceeds(s.cfg, now) {
		return 0, ctx.Err()
	}
	removed := 0
	var after usage
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		var err error
		removed, after, err = s.evict(ctx, tx, now)
		return err
	})
	if err != nil {
		s.exact = false
		return 0, err
	}
	s.usage = after
	return removed, nil
}
