package proxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	genv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const grpcTimeout = 10 * time.Second

// grpcProxy implements AgentProxy over gRPC (tno.v1.TnoApi).
type grpcProxy struct {
	endpoint string
}

func (p *grpcProxy) ListTasks(ctx context.Context, f TaskFilter) (*TaskListResult, error) {
	ctx, cancel := context.WithTimeout(ctx, grpcTimeout)
	defer cancel()

	conn, err := grpc.NewClient(
		p.endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("proxy: grpc dial %s: %w", p.endpoint, err)
	}
	defer conn.Close()

	client := genv1.NewTnoApiClient(conn)

	req := &genv1.ListTasksRequest{
		Limit:  uint32(f.Limit),
		Offset: uint32(f.Offset),
	}
	if f.Slot != "" {
		req.Slot = &f.Slot
	}
	if f.Status != "" {
		if s, ok := parseTaskStatus(f.Status); ok {
			req.Status = &s
		}
	}

	resp, err := client.ListTasks(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("proxy: grpc ListTasks: %w", err)
	}

	tasks := make([]Task, len(resp.GetTasks()))
	for i, t := range resp.GetTasks() {
		tasks[i] = Task{
			ID:        t.GetId(),
			Slot:      t.GetSlot(),
			Status:    taskStatusString(t.GetStatus()),
			Attempt:   int(t.GetAttempt()),
			CreatedAt: t.GetCreatedAt(),
			UpdatedAt: t.GetUpdatedAt(),
			Error:     t.GetError(),
		}
	}

	return &TaskListResult{
		Tasks: tasks,
		Total: int(resp.GetTotal()),
	}, nil
}

// taskStatusString converts a proto TaskStatus enum to a lowercase string.
//
//	TASK_STATUS_RUNNING → "running"
func taskStatusString(s genv1.TaskStatus) string {
	name := s.String() // "TASK_STATUS_RUNNING"
	name = strings.TrimPrefix(name, "TASK_STATUS_")
	return strings.ToLower(name)
}

// parseTaskStatus converts a lowercase status string to the proto enum.
//
//	"running" → TASK_STATUS_RUNNING
func parseTaskStatus(s string) (genv1.TaskStatus, bool) {
	key := "TASK_STATUS_" + strings.ToUpper(s)
	v, ok := genv1.TaskStatus_value[key]
	if !ok {
		return genv1.TaskStatus_TASK_STATUS_UNSPECIFIED, false
	}
	return genv1.TaskStatus(v), true
}
