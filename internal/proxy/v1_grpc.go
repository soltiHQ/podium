package proxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	genv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"google.golang.org/grpc"
)

const grpcV1Timeout = 10 * time.Second

// grpcProxyV1 implements AgentProxy over gRPC (solti.v1.SoltiApi).
type grpcProxyV1 struct {
	conn *grpc.ClientConn
}

func (p *grpcProxyV1) ListTasks(ctx context.Context, f TaskFilter) (*proxyv1.TaskListResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, grpcV1Timeout)
	defer cancel()

	client := genv1.NewSoltiApiClient(p.conn)

	req := &genv1.ListTasksRequest{
		Limit:  uint32(f.Limit),
		Offset: uint32(f.Offset),
	}
	if f.Slot != "" {
		req.Slot = &f.Slot
	}
	if f.Status != "" {
		if s, ok := parseV1TaskStatus(f.Status); ok {
			req.Status = &s
		}
	}

	resp, err := client.ListTasks(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrListTasks, err)
	}

	tasks := make([]proxyv1.Task, len(resp.GetTasks()))
	for i, t := range resp.GetTasks() {
		tasks[i] = proxyv1.Task{
			ID:        t.GetId(),
			Slot:      t.GetSlot(),
			Status:    v1TaskStatusString(t.GetStatus()),
			Attempt:   int(t.GetAttempt()),
			CreatedAt: t.GetCreatedAt(),
			UpdatedAt: t.GetUpdatedAt(),
			Error:     t.GetError(),
		}
	}

	return &proxyv1.TaskListResponse{
		Tasks: tasks,
		Total: int(resp.GetTotal()),
	}, nil
}

// v1TaskStatusString converts a v1 proto TaskStatus enum to a lowercase string.
//
//	TASK_STATUS_RUNNING → "running"
func v1TaskStatusString(s genv1.TaskStatus) string {
	name := s.String()
	name = strings.TrimPrefix(name, "TASK_STATUS_")
	return strings.ToLower(name)
}

// parseV1TaskStatus converts a lowercase status string to the v1 proto enum.
//
//	"running" → TASK_STATUS_RUNNING
func parseV1TaskStatus(s string) (genv1.TaskStatus, bool) {
	key := "TASK_STATUS_" + strings.ToUpper(s)
	v, ok := genv1.TaskStatus_value[key]
	if !ok {
		return genv1.TaskStatus_TASK_STATUS_UNSPECIFIED, false
	}
	return genv1.TaskStatus(v), true
}
