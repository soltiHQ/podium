package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	genv1 "github.com/soltiHQ/control-plane/api/gen/v1"
	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
)

const (
	v1PathTasks = "/api/v1/tasks"
)

// httpV1Unmarshal decodes canonical proto-JSON responses from the SDK.
// DiscardUnknown keeps forward compatibility when newer agents add fields.
var httpV1Unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// httpV1Marshal produces canonical proto-JSON request bodies.
var httpV1Marshal = protojson.MarshalOptions{
	UseProtoNames:   false,
	EmitUnpopulated: false,
}

// httpProxyV1 implements AgentProxy over HTTP for API v1.
type httpProxyV1 struct {
	endpoint string
	client   httpClient
}

func (p *httpProxyV1) ListTasks(ctx context.Context, f TaskFilter) (*proxyv1.TaskListResponse, error) {
	u, err := url.Parse(p.endpoint + v1PathTasks)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadEndpointURL, err)
	}

	q := u.Query()
	if f.Slot != "" {
		q.Set("slot", f.Slot)
	}
	if f.Status != "" {
		q.Set("status", f.Status)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Offset > 0 {
		q.Set("offset", strconv.Itoa(f.Offset))
	}
	u.RawQuery = q.Encode()

	var out genv1.ListTasksResponse
	if err := doProtoJSONGet(ctx, p.client, u.String(), &out); err != nil {
		return nil, err
	}

	tasks := make([]proxyv1.Task, len(out.GetTasks()))
	for i, t := range out.GetTasks() {
		tasks[i] = taskDataToProxy(t)
	}

	return &proxyv1.TaskListResponse{
		Tasks: tasks,
		Total: int(out.GetTotal()),
	}, nil
}

func (p *httpProxyV1) SubmitTask(ctx context.Context, sub TaskSubmission) (string, error) {
	if sub.Spec == nil {
		return "", fmt.Errorf("%w: nil spec", ErrSubmitTask)
	}
	u, err := url.Parse(p.endpoint + v1PathTasks)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadEndpointURL, err)
	}

	// SubmitTaskResponse carries the TaskId the agent assigned. Without
	// it CP cannot later DeleteTask / GetTaskStatus for this exact run,
	// which breaks the update and uninstall flows.
	var out genv1.SubmitTaskResponse
	if err := doProtoJSONPostDecoding(ctx, p.client, u.String(), &genv1.SubmitTaskRequest{Spec: sub.Spec}, &out); err != nil {
		return "", err
	}
	taskID := out.GetTaskId()
	if taskID == "" {
		return "", fmt.Errorf("%w: agent returned empty task id", ErrSubmitTask)
	}
	return taskID, nil
}

func (p *httpProxyV1) GetTask(ctx context.Context, taskID string) (*proxyv1.TaskStatusResponse, error) {
	u, err := url.Parse(fmt.Sprintf("%s%s/%s", p.endpoint, v1PathTasks, taskID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadEndpointURL, err)
	}

	var out genv1.GetTaskStatusResponse
	if err := doProtoJSONGet(ctx, p.client, u.String(), &out); err != nil {
		return nil, err
	}

	var task *proxyv1.Task
	if data := out.GetTask(); data != nil {
		t := taskDataToProxy(data)
		task = &t
	}
	return &proxyv1.TaskStatusResponse{Info: task}, nil
}

func (p *httpProxyV1) ListTaskRuns(ctx context.Context, taskID string) (*proxyv1.TaskRunListResponse, error) {
	u, err := url.Parse(fmt.Sprintf("%s%s/%s/runs", p.endpoint, v1PathTasks, taskID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadEndpointURL, err)
	}

	var out genv1.ListTaskRunsResponse
	if err := doProtoJSONGet(ctx, p.client, u.String(), &out); err != nil {
		return nil, err
	}

	runs := make([]proxyv1.TaskRun, len(out.GetRuns()))
	for i, r := range out.GetRuns() {
		run := proxyv1.TaskRun{
			Attempt:   int(r.GetAttempt()),
			Status:    v1TaskStatusString(r.GetStatus()),
			StartedAt: r.GetStartedAt(),
		}
		if r.FinishedAt != nil {
			run.FinishedAt = *r.FinishedAt
		}
		if r.Error != nil {
			run.Error = *r.Error
		}
		if r.ExitCode != nil {
			run.ExitCode = r.ExitCode
		}
		runs[i] = run
	}
	return &proxyv1.TaskRunListResponse{Runs: runs}, nil
}

func (p *httpProxyV1) DeleteTask(ctx context.Context, taskID string) error {
	u, err := url.Parse(fmt.Sprintf("%s%s/%s", p.endpoint, v1PathTasks, taskID))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadEndpointURL, err)
	}

	return doDelete(ctx, p.client, u.String())
}

// doProtoJSONGet performs a GET and decodes the response as proto-JSON.
// On non-200 responses the SDK error envelope (`{"error","message"}`) is
// surfaced via formatUnexpectedStatus instead of being silently discarded.
func doProtoJSONGet(ctx context.Context, client httpClient, url string, out proto.Message) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCreateRequest, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRequest, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return formatUnexpectedStatus(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDecode, err)
	}
	if err := httpV1Unmarshal.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %v", ErrDecode, err)
	}
	return nil
}

