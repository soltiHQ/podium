package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
)

const (
	v1PathTasks           = "/apis/solti.io/v1/tasks"
	maxAgentResponseBytes = 4 * 1024 * 1024
)

type httpProxyV1 struct {
	endpoint string
	client   httpClient
}
type resourceMeta struct {
	Name            string    `json:"name"`
	UID             string    `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
	Generation      uint64    `json:"generation"`
	CreatedAt       time.Time `json:"creationTimestamp"`
}
type taskConditionResource struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	ObservedGeneration uint64     `json:"observedGeneration"`
	LastTransitionTime *time.Time `json:"lastTransitionTime"`
	Reason             string     `json:"reason"`
	Message            string     `json:"message"`
}

type taskResource struct {
	Metadata resourceMeta `json:"metadata"`
	Spec     struct {
		Slot string `json:"slot"`
	} `json:"spec"`
	Status struct {
		Phase              string                  `json:"phase"`
		Attempt            int                     `json:"attempt"`
		Error              string                  `json:"error"`
		ExitCode           *int32                  `json:"exitCode"`
		ObservedGeneration uint64                  `json:"observedGeneration"`
		Conditions         []taskConditionResource `json:"conditions"`
	} `json:"status"`
}

func (t taskResource) flat() proxyv1.Task {
	conditions := make([]proxyv1.TaskCondition, 0, len(t.Status.Conditions))
	for _, condition := range t.Status.Conditions {
		item := proxyv1.TaskCondition{Type: condition.Type, Status: condition.Status, ObservedGeneration: condition.ObservedGeneration, Reason: condition.Reason, Message: condition.Message}
		if condition.LastTransitionTime != nil {
			ms := condition.LastTransitionTime.UnixMilli()
			item.LastTransitionTime = &ms
		}
		conditions = append(conditions, item)
	}
	createdAt := int64(0)
	if !t.Metadata.CreatedAt.IsZero() {
		createdAt = t.Metadata.CreatedAt.UnixMilli()
	}
	return proxyv1.Task{Conditions: conditions, ID: t.Metadata.Name, UID: t.Metadata.UID, ResourceVersion: t.Metadata.ResourceVersion, Generation: t.Metadata.Generation, ObservedGeneration: t.Status.ObservedGeneration, CreatedAt: createdAt, UpdatedAt: 0, Slot: t.Spec.Slot, Status: t.Status.Phase, Attempt: t.Status.Attempt, Error: t.Status.Error, ExitCode: t.Status.ExitCode}
}

type listMeta struct {
	Next            string  `json:"continue"`
	ResourceVersion string  `json:"resourceVersion"`
	Remaining       *uint64 `json:"remainingItemCount"`
	TaskUID         string  `json:"taskUid"`
}

func (p *httpProxyV1) taskURL(name, suffix string) string {
	return p.endpoint + v1PathTasks + "/" + url.PathEscape(name) + suffix
}

func (p *httpProxyV1) ListTasks(ctx context.Context, f TaskFilter) (*proxyv1.ListTasksResponse, error) {
	if err := validateTaskFilter(f); err != nil {
		return nil, err
	}
	if f.Offset > 0 {
		return nil, fmt.Errorf("%w: offset pagination unsupported; use cursor", ErrListTasks)
	}
	u, err := url.Parse(p.endpoint + v1PathTasks)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadEndpointURL, err)
	}
	q := u.Query()
	if f.Slot != "" {
		q.Set("slot", f.Slot)
	}
	if f.Status != "" {
		q.Add("phase", f.Status)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Cursor != "" {
		q.Set("continue", f.Cursor)
	}
	if f.LabelSelector != "" {
		q.Set("labelSelector", f.LabelSelector)
	}
	u.RawQuery = q.Encode()
	var out struct {
		Metadata listMeta       `json:"metadata"`
		Items    []taskResource `json:"items"`
	}
	if err := p.request(ctx, http.MethodGet, u.String(), nil, &out); err != nil {
		return nil, agentError(ErrListTasks, err)
	}
	tasks := make([]proxyv1.Task, len(out.Items))
	for i, t := range out.Items {
		tasks[i] = t.flat()
	}
	return &proxyv1.ListTasksResponse{Tasks: tasks, Total: len(tasks), NextCursor: out.Metadata.Next, ResourceVersion: out.Metadata.ResourceVersion, RemainingCount: out.Metadata.Remaining}, nil
}

func (p *httpProxyV1) ApplyTask(ctx context.Context, sub TaskSubmission) (string, error) {
	if sub.Spec == nil || sub.Spec.GetMetadata().GetName() == "" {
		return "", fmt.Errorf("%w: missing manifest name", ErrApplyTask)
	}
	body, err := TaskManifestJSON(sub.Spec)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrApplyTask, err)
	}
	u := p.taskURL(sub.Spec.GetMetadata().GetName(), "")
	if pre := sub.Preconditions; pre != nil {
		parsed, _ := url.Parse(u)
		q := parsed.Query()
		if pre.Uid != nil {
			q.Set("uid", *pre.Uid)
		}
		if pre.ResourceVersion != nil {
			q.Set("resourceVersion", *pre.ResourceVersion)
		}
		parsed.RawQuery = q.Encode()
		u = parsed.String()
	}
	var out taskResource
	if err := p.request(ctx, http.MethodPut, u, body, &out); err != nil {
		return "", agentError(ErrApplyTask, err)
	}
	if out.Metadata.Name == "" {
		return "", fmt.Errorf("%w: agent returned empty task id", ErrApplyTask)
	}
	if out.Metadata.Name != sub.Spec.GetMetadata().GetName() {
		return "", fmt.Errorf("%w: agent returned different task name", ErrApplyTask)
	}
	if out.Metadata.UID == "" {
		return "", fmt.Errorf("%w: agent returned empty task UID", ErrApplyTask)
	}
	return out.Metadata.Name, nil
}

func (p *httpProxyV1) GetTask(ctx context.Context, name string) (*proxyv1.GetTaskResponse, error) {
	var out taskResource
	if err := p.request(ctx, http.MethodGet, p.taskURL(name, ""), nil, &out); err != nil {
		return nil, agentError(ErrGetTask, err)
	}
	if out.Metadata.Name != name || out.Metadata.UID == "" {
		return nil, fmt.Errorf("%w: invalid task identity", ErrGetTask)
	}
	t := out.flat()
	return &proxyv1.GetTaskResponse{Task: &t}, nil
}

func (p *httpProxyV1) ListTaskRuns(ctx context.Context, name string) (*proxyv1.ListTaskRunsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return aggregateTaskRuns(ctx, p, name)
}

func (p *httpProxyV1) writeTask(ctx context.Context, name string, cancel bool) error {
	task, err := p.GetTask(ctx, name)
	if err != nil {
		return err
	}
	if task.Task == nil || task.Task.UID == "" {
		return fmt.Errorf("%w: missing task UID", ErrDeleteTask)
	}
	suffix := ""
	method := http.MethodDelete
	if cancel {
		suffix = "/cancel"
		method = http.MethodPost
	}
	parsed, _ := url.Parse(p.taskURL(name, suffix))
	q := parsed.Query()
	q.Set("uid", task.Task.UID)
	parsed.RawQuery = q.Encode()
	if err := p.request(ctx, method, parsed.String(), nil, nil); err != nil {
		return agentError(ErrDeleteTask, err)
	}
	return nil
}

func (p *httpProxyV1) DeleteTask(ctx context.Context, name string) error {
	return p.writeTask(ctx, name, false)
}

func (p *httpProxyV1) CancelTask(ctx context.Context, name string) error {
	return p.writeTask(ctx, name, true)
}

func (p *httpProxyV1) request(ctx context.Context, method, target string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCreateRequest, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRequest, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return formatUnexpectedStatus(resp)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAgentResponseBytes))
		return err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDecode, err)
	}
	if len(data) > maxAgentResponseBytes {
		return fmt.Errorf("%w: agent response exceeds 4 MiB", ErrDecode)
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: %v", ErrDecode, err)
	}
	return nil
}

func (p *httpProxyV1) StreamTaskLogs(ctx context.Context, name string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
	task, err := p.GetTask(ctx, name)
	if err != nil {
		return nil, err
	}
	return p.StreamTaskLogsForUID(ctx, name, task.Task.UID)
}

func (p *httpProxyV1) StreamTaskLogsForUID(ctx context.Context, name, uid string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
	if uid == "" {
		return nil, fmt.Errorf("%w: missing task UID", ErrStreamTaskLogs)
	}
	parsed, _ := url.Parse(p.taskURL(name, "/logs"))
	q := parsed.Query()
	q.Set("taskUid", uid)
	parsed.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCreateRequest, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStreamTaskLogs, err)
	}
	if resp.StatusCode != http.StatusOK {
		err := formatUnexpectedStatus(resp)
		resp.Body.Close()
		return nil, agentError(ErrStreamTaskLogs, err)
	}
	ch := make(chan *taskv1.StreamTaskLogsResponse, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		var data []string
		eventName := ""
		frameBytes := 0
		for scanner.Scan() {
			line := strings.TrimSuffix(scanner.Text(), "\r")
			if line == "" {
				if len(data) > 0 {
					ev := parseAgentLogFrame(eventName, strings.Join(data, "\n"), uid)
					if ev != nil {
						select {
						case ch <- ev:
						case <-ctx.Done():
							return
						}
					}
				}
				data = nil
				eventName = ""
				frameBytes = 0
				continue
			}
			if strings.HasPrefix(line, "event:") {
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
			if strings.HasPrefix(line, "data:") {
				frameBytes += len(line)
				if frameBytes > 1024*1024 {
					return
				}
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
	}()
	return ch, nil
}

type agentLogEvent struct {
	Type         string `json:"type"`
	TaskUID      string `json:"taskUid"`
	Generation   uint64 `json:"generation"`
	Attempt      uint32 `json:"attempt"`
	Stream       string `json:"stream"`
	Seq          uint64 `json:"seq"`
	Ts           int64  `json:"ts"`
	Line         string `json:"line"`
	Truncated    bool   `json:"truncated"`
	Started      int64  `json:"startedAt"`
	Finished     int64  `json:"finishedAt"`
	ExitCode     *int32 `json:"exitCode"`
	Skipped      uint64 `json:"skipped"`
	SkippedBytes uint64 `json:"skippedBytes"`
}

func parseAgentLogFrame(eventName, payload, uid string) *taskv1.StreamTaskLogsResponse {
	var e agentLogEvent
	if json.Unmarshal([]byte(payload), &e) != nil {
		return nil
	}
	if uid != "" && e.TaskUID != uid {
		return nil
	}
	if eventName != "" {
		want := map[string]string{"chunk": "chunk", "run-started": "runStarted", "run-finished": "runFinished", "lagged": "lagged"}[eventName]
		if want == "" || want != e.Type {
			return nil
		}
	}
	switch e.Type {
	case "chunk":
		line, err := base64.StdEncoding.DecodeString(e.Line)
		if err != nil {
			return nil
		}
		return &taskv1.StreamTaskLogsResponse{Kind: &taskv1.StreamTaskLogsResponse_Chunk{Chunk: &taskv1.OutputChunk{Generation: e.Generation, Attempt: e.Attempt, Stream: agentStreamToProto(e.Stream), Seq: e.Seq, Ts: e.Ts, Line: line, Truncated: e.Truncated}}}
	case "runStarted":
		return &taskv1.StreamTaskLogsResponse{Kind: &taskv1.StreamTaskLogsResponse_RunStarted{RunStarted: &taskv1.RunStarted{Generation: e.Generation, Attempt: e.Attempt, StartedAt: e.Started}}}
	case "runFinished":
		return &taskv1.StreamTaskLogsResponse{Kind: &taskv1.StreamTaskLogsResponse_RunFinished{RunFinished: &taskv1.RunFinished{Generation: e.Generation, Attempt: e.Attempt, FinishedAt: e.Finished, ExitCode: e.ExitCode}}}
	case "lagged":
		return &taskv1.StreamTaskLogsResponse{Kind: &taskv1.StreamTaskLogsResponse_Lagged{Lagged: &taskv1.Lagged{Skipped: e.Skipped, SkippedBytes: e.SkippedBytes}}}
	}
	return nil
}

func agentStreamToProto(s string) taskv1.OutputStreamKind {
	switch s {
	case "stdout":
		return taskv1.OutputStreamKind_OUTPUT_STREAM_KIND_STDOUT
	case "stderr":
		return taskv1.OutputStreamKind_OUTPUT_STREAM_KIND_STDERR
	}
	return taskv1.OutputStreamKind_OUTPUT_STREAM_KIND_UNSPECIFIED
}
