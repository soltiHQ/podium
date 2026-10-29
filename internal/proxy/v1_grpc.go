package proxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// grpcProxyV1 implements AgentProxy over gRPC (solti.task.v1.TaskService).
type grpcProxyV1 struct {
	conn  *grpc.ClientConn
	token string
}

// authCtx attaches the bearer token as gRPC "authorization" metadata when set.
// Empty token → ctx unchanged (no credential sent).
func (p *grpcProxyV1) authCtx(ctx context.Context) context.Context {
	if p.token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+p.token)
}

func (p *grpcProxyV1) ListTasks(ctx context.Context, f TaskFilter) (*proxyv1.ListTasksResponse, error) {
	if err := validateTaskFilter(f); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if f.Offset > 0 {
		return nil, fmt.Errorf("%w: offset pagination unsupported; use cursor", ErrListTasks)
	}
	client := taskv1.NewTaskServiceClient(p.conn)

	req := &taskv1.ListTasksRequest{
		Limit: clampUint32(f.Limit), Continue: f.Cursor, LabelSelector: f.LabelSelector,
	}
	if f.Slot != "" {
		req.Slot = &f.Slot
	}
	if f.Status != "" {
		if s, ok := parseV1TaskStatus(f.Status); ok {
			req.Phases = []taskv1.TaskPhase{s}
		} else {
			return nil, fmt.Errorf("%w: invalid task phase %q", ErrListTasks, f.Status)
		}
	}

	resp, err := client.ListTasks(p.authCtx(ctx), req)
	if err != nil {
		return nil, agentError(ErrListTasks, err)
	}

	tasks := make([]proxyv1.Task, len(resp.GetTasks()))
	for i, t := range resp.GetTasks() {
		tasks[i] = taskDataToProxy(t)
	}

	return &proxyv1.ListTasksResponse{
		Tasks: tasks,
		Total: len(tasks), NextCursor: resp.GetContinue(), ResourceVersion: resp.GetResourceVersion(), RemainingCount: resp.RemainingItemCount,
	}, nil
}

func (p *grpcProxyV1) ApplyTask(ctx context.Context, sub TaskSubmission) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if sub.Spec == nil {
		return "", fmt.Errorf("%w: nil spec", ErrApplyTask)
	}
	client := taskv1.NewTaskServiceClient(p.conn)

	resp, err := client.ApplyTask(p.authCtx(ctx), &taskv1.ApplyTaskRequest{Manifest: sub.Spec, Preconditions: sub.Preconditions})
	if err != nil {
		return "", agentError(ErrApplyTask, err)
	}
	taskID := resp.GetTask().GetMetadata().GetName()
	if taskID == "" {
		return "", fmt.Errorf("%w: agent returned empty task id", ErrApplyTask)
	}
	if taskID != sub.Spec.GetMetadata().GetName() {
		return "", fmt.Errorf("%w: agent returned different task name", ErrApplyTask)
	}
	if resp.GetTask().GetMetadata().GetUid() == "" {
		return "", fmt.Errorf("%w: agent returned empty task UID", ErrApplyTask)
	}
	return taskID, nil
}

func (p *grpcProxyV1) GetTask(ctx context.Context, taskID string) (*proxyv1.GetTaskResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := taskv1.NewTaskServiceClient(p.conn)

	resp, err := client.GetTask(p.authCtx(ctx), &taskv1.GetTaskRequest{Name: taskID})
	if err != nil {
		return nil, agentError(ErrGetTask, err)
	}

	if resp.GetTask() == nil || resp.GetTask().GetMetadata().GetName() != taskID || resp.GetTask().GetMetadata().GetUid() == "" {
		return nil, fmt.Errorf("%w: invalid task identity", ErrGetTask)
	}
	var task *proxyv1.Task
	if data := resp.GetTask(); data != nil {
		t := taskDataToProxy(data)
		task = &t
	}

	return &proxyv1.GetTaskResponse{Task: task}, nil
}

func (p *grpcProxyV1) ListTaskRuns(ctx context.Context, taskID string) (*proxyv1.ListTaskRunsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return aggregateTaskRuns(ctx, p, taskID)
}

func (p *grpcProxyV1) DeleteTask(ctx context.Context, taskID string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	task, err := p.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.Task == nil || task.Task.UID == "" {
		return fmt.Errorf("%w: missing task UID", ErrDeleteTask)
	}
	_, err = taskv1.NewTaskServiceClient(p.conn).DeleteTask(p.authCtx(ctx), &taskv1.DeleteTaskRequest{Name: taskID, Preconditions: &taskv1.WritePreconditions{Uid: &task.Task.UID}})
	if err != nil {
		return agentError(ErrDeleteTask, err)
	}
	return nil
}

func (p *grpcProxyV1) CancelTask(ctx context.Context, taskID string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	task, err := p.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.Task == nil || task.Task.UID == "" {
		return fmt.Errorf("%w: missing task UID", ErrDeleteTask)
	}
	_, err = taskv1.NewTaskServiceClient(p.conn).CancelTask(p.authCtx(ctx), &taskv1.CancelTaskRequest{Name: taskID, Preconditions: &taskv1.WritePreconditions{Uid: &task.Task.UID}})
	if err != nil {
		return agentError(ErrDeleteTask, err)
	}
	return nil
}

// StreamTaskLogs opens the agent's StreamTaskLogs server-stream, forwards
// each StreamTaskLogsResponse into a channel, and closes it on EOF, ctx
// cancellation, or transport error. Channel buffer is 64 — typical chunk
// cadence (~10 lines/sec) is well below; bursts are absorbed.
func (p *grpcProxyV1) StreamTaskLogs(ctx context.Context, taskID string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
	task, err := p.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return p.StreamTaskLogsForUID(ctx, taskID, task.Task.UID)
}

func (p *grpcProxyV1) StreamTaskLogsForUID(ctx context.Context, taskID, uid string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
	if uid == "" {
		return nil, fmt.Errorf("%w: missing task UID", ErrStreamTaskLogs)
	}
	client := taskv1.NewTaskServiceClient(p.conn)
	stream, err := client.StreamTaskLogs(p.authCtx(ctx), &taskv1.StreamTaskLogsRequest{Name: taskID, TaskUid: uid})
	if err != nil {
		return nil, agentError(ErrStreamTaskLogs, err)
	}
	ch := make(chan *taskv1.StreamTaskLogsResponse, 64)
	go func() {
		defer close(ch)
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// taskDataToProxy converts a proto TaskData (nested metadata + spec + status)
// into the flat proxy-level Task type consumed by podium's own REST/UI.
func taskDataToProxy(t *taskv1.Task) proxyv1.Task {
	meta := t.GetMetadata()
	st := t.GetStatus()
	spec := t.GetSpec()

	task := proxyv1.Task{
		ID:        meta.GetName(),
		Slot:      spec.GetSlot(),
		Status:    v1TaskStatusString(st.GetPhase()),
		Attempt:   int(st.GetAttempt()),
		CreatedAt: meta.GetCreationTimestamp(), UID: meta.GetUid(), Generation: meta.GetGeneration(), ObservedGeneration: st.GetObservedGeneration(),
		UpdatedAt:       0,
		Error:           st.GetError(),
		ResourceVersion: meta.GetResourceVersion(),
		Conditions:      make([]proxyv1.TaskCondition, 0, len(st.GetConditions())),
	}
	for _, condition := range st.GetConditions() {
		ms := condition.GetLastTransitionTime()
		task.Conditions = append(task.Conditions, proxyv1.TaskCondition{Type: condition.GetType(), Status: conditionStatusString(condition.GetStatus()), ObservedGeneration: condition.GetObservedGeneration(), LastTransitionTime: &ms, Reason: condition.GetReason(), Message: condition.GetMessage()})
	}
	if st != nil && st.ExitCode != nil {
		task.ExitCode = st.ExitCode
	}
	return task
}

// v1TaskStatusString converts a v1 proto TaskStatus enum to a lowercase string.
//
//	TASK_PHASE_RUNNING → "running"
func v1TaskStatusString(s taskv1.TaskPhase) string {
	name := s.String()
	name = strings.TrimPrefix(name, "TASK_PHASE_")
	return strings.ToLower(name)
}

// parseV1TaskStatus converts a lowercase status string to the v1 proto enum.
//
//	"running" → TASK_PHASE_RUNNING
func parseV1TaskStatus(s string) (taskv1.TaskPhase, bool) {
	key := "TASK_PHASE_" + strings.ToUpper(s)
	v, ok := taskv1.TaskPhase_value[key]
	if !ok {
		return taskv1.TaskPhase_TASK_PHASE_UNSPECIFIED, false
	}
	return taskv1.TaskPhase(v), true
}

func clampUint32(v int) uint32 {
	if v <= 0 {
		return 0
	}
	return uint32(v)
}
