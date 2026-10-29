package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
)

// TaskRunPager exposes one bounded SDK snapshot page. Callers must validate
// TaskUID and ResourceVersion across pages rather than join different snapshots.
type TaskRunPager interface {
	ListTaskRunsPage(context.Context, string, TaskRunQuery) (*TaskRunPage, error)
}
type TaskRunQuery struct {
	Limit  uint32
	Cursor string
}
type TaskRunPage struct {
	Runs                                 []proxyv1.TaskRun
	TaskUID, ResourceVersion, NextCursor string
}

var ErrRunSnapshotExpired = errors.New("proxy: run snapshot expired")

const (
	maxAggregateRunPages = 16
	maxAggregateRunRows  = 1024
	maxAggregateRunBytes = 8 * 1024 * 1024
)

// aggregateTaskRuns supports the existing live-history inspector. Payload
// accounting charges 128 bytes of scalar/row overhead plus all retained string
// bytes and page metadata; it is a logical bound, not exact heap accounting.
// A larger source must use the agent's paginated API or the retained archive.
// No partial response is returned as though it were the complete history.
func aggregateTaskRuns(ctx context.Context, pager TaskRunPager, name string) (*proxyv1.ListTaskRunsResponse, error) {
	out := &proxyv1.ListTaskRunsResponse{Runs: make([]proxyv1.TaskRun, 0)}
	cursor, version := "", ""
	seen := map[string]bool{"": true}
	var bytes int64
	for pageNumber := 0; pageNumber < maxAggregateRunPages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := pager.ListTaskRunsPage(ctx, name, TaskRunQuery{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if page == nil || page.TaskUID == "" || page.ResourceVersion == "" ||
			(out.TaskUID != "" && out.TaskUID != page.TaskUID) || (version != "" && version != page.ResourceVersion) {
			return nil, fmt.Errorf("%w: run snapshot changed or missing", ErrListTaskRuns)
		}
		if len(page.Runs) > 1000 {
			return nil, fmt.Errorf("%w: agent run page exceeds requested 1000 rows", ErrListTaskRuns)
		}
		if len(out.Runs)+len(page.Runs) > maxAggregateRunRows {
			return nil, fmt.Errorf("%w: live history exceeds %d runs; use the paginated agent run API or retained archive", ErrListTaskRuns, maxAggregateRunRows)
		}
		bytes += int64(len(page.TaskUID)) + int64(len(page.ResourceVersion)) + int64(len(page.NextCursor))
		for _, run := range page.Runs {
			bytes += 128 + int64(len(run.Status)) + int64(len(run.Error)) +
				int64(len(run.WorkloadAPIVersion)) + int64(len(run.WorkloadKind))
		}
		if bytes > maxAggregateRunBytes {
			return nil, fmt.Errorf("%w: live history exceeds 8 MiB logical payload; use the paginated agent run API or retained archive", ErrListTaskRuns)
		}
		out.TaskUID, version = page.TaskUID, page.ResourceVersion
		out.Runs = append(out.Runs, page.Runs...)
		cursor = page.NextCursor
		if cursor == "" {
			return out, nil
		}
		if seen[cursor] {
			return nil, fmt.Errorf("%w: repeated cursor", ErrListTaskRuns)
		}
		seen[cursor] = true
		if len(out.Runs) == maxAggregateRunRows {
			return nil, fmt.Errorf("%w: live history exceeds %d runs; use the paginated agent run API or retained archive", ErrListTaskRuns, maxAggregateRunRows)
		}
	}
	return nil, fmt.Errorf("%w: live history exceeds %d pages; use the paginated agent run API or retained archive", ErrListTaskRuns, maxAggregateRunPages)
}

func (p *httpProxyV1) ListTaskRunsPage(ctx context.Context, name string, q TaskRunQuery) (*TaskRunPage, error) {
	if q.Limit < 1 || q.Limit > 1000 {
		return nil, fmt.Errorf("%w: run page limit must be 1..1000", ErrListTaskRuns)
	}
	parsed, err := url.Parse(p.taskURL(name, "/runs"))
	if err != nil {
		return nil, err
	}
	params := parsed.Query()
	params.Set("limit", strconv.FormatUint(uint64(q.Limit), 10))
	if q.Cursor != "" {
		params.Set("continue", q.Cursor)
	}
	parsed.RawQuery = params.Encode()
	var page struct {
		Metadata listMeta `json:"metadata"`
		Runs     []struct {
			Generation uint64     `json:"generation"`
			Attempt    int        `json:"attempt"`
			Phase      string     `json:"phase"`
			StartedAt  *time.Time `json:"startedAt"`
			FinishedAt *time.Time `json:"finishedAt"`
			Error      string     `json:"error"`
			ExitCode   *int32     `json:"exitCode"`
			Workload   struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
			} `json:"workload"`
		} `json:"runs"`
	}
	if err := p.request(ctx, http.MethodGet, parsed.String(), nil, &page); err != nil {
		var response interface{ HTTPStatus() int }
		if errors.As(err, &response) && response.HTTPStatus() == http.StatusGone {
			return nil, fmt.Errorf("%w: %w", ErrRunSnapshotExpired, agentError(ErrListTaskRuns, err))
		}
		return nil, agentError(ErrListTaskRuns, err)
	}
	out := &TaskRunPage{TaskUID: page.Metadata.TaskUID, ResourceVersion: page.Metadata.ResourceVersion, NextCursor: page.Metadata.Next, Runs: make([]proxyv1.TaskRun, 0, len(page.Runs))}
	for _, r := range page.Runs {
		run := proxyv1.TaskRun{Generation: r.Generation, Attempt: r.Attempt, Status: r.Phase, StartedAt: 0, Error: r.Error, ExitCode: r.ExitCode, WorkloadAPIVersion: r.Workload.APIVersion, WorkloadKind: r.Workload.Kind, FinishedAtPresent: r.FinishedAt != nil}
		if r.StartedAt != nil {
			run.StartedAt = r.StartedAt.UnixMilli()
		}
		if r.FinishedAt != nil {
			run.FinishedAt = r.FinishedAt.UnixMilli()
		}
		out.Runs = append(out.Runs, run)
	}
	return out, nil
}

func (p *grpcProxyV1) ListTaskRunsPage(ctx context.Context, name string, q TaskRunQuery) (*TaskRunPage, error) {
	if q.Limit < 1 || q.Limit > 1000 {
		return nil, fmt.Errorf("%w: run page limit must be 1..1000", ErrListTaskRuns)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	page, err := taskv1.NewTaskServiceClient(p.conn).ListTaskRuns(p.authCtx(ctx), &taskv1.ListTaskRunsRequest{Name: name, Limit: q.Limit, Continue: q.Cursor})
	if err != nil {
		if status.Code(err) == codes.OutOfRange {
			return nil, fmt.Errorf("%w: %w", ErrRunSnapshotExpired, agentError(ErrListTaskRuns, err))
		}
		return nil, agentError(ErrListTaskRuns, err)
	}
	out := &TaskRunPage{TaskUID: page.GetTaskUid(), ResourceVersion: page.GetResourceVersion(), NextCursor: page.GetContinue(), Runs: make([]proxyv1.TaskRun, 0, len(page.GetRuns()))}
	for _, r := range page.GetRuns() {
		out.Runs = append(out.Runs, proxyv1.TaskRun{Generation: r.GetGeneration(), Attempt: int(r.GetAttempt()), Status: v1TaskStatusString(r.GetPhase()), StartedAt: r.GetStartedAt(), FinishedAt: r.GetFinishedAt(), FinishedAtPresent: r.FinishedAt != nil, Error: r.GetError(), ExitCode: r.ExitCode, WorkloadAPIVersion: r.GetWorkloadApiVersion(), WorkloadKind: r.GetWorkloadKind()})
	}
	return out, nil
}
