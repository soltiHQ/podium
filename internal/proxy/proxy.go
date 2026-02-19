// Package proxy provides outbound communication with agents.
//
// The control-plane calls INTO agents to query tasks, submit work, etc.
// Each agent exposes either an HTTP or a gRPC endpoint; the proxy package
// auto-selects the transport based on the endpoint scheme.
package proxy

import (
	"context"
	"strings"
)

// Task is the proxy-internal representation of an agent task.
type Task struct {
	ID        string
	Slot      string
	Status    string
	Attempt   int
	CreatedAt int64
	UpdatedAt int64
	Error     string
}

// TaskFilter holds optional filters and pagination for listing tasks.
type TaskFilter struct {
	Slot   string
	Status string
	Limit  int
	Offset int
}

// TaskListResult is the result of a ListTasks call.
type TaskListResult struct {
	Tasks []Task
	Total int
}

// AgentProxy is the interface for outbound communication with an agent.
type AgentProxy interface {
	ListTasks(ctx context.Context, filter TaskFilter) (*TaskListResult, error)
}

// New creates an HTTP or gRPC proxy depending on the endpoint scheme.
//
// If the endpoint starts with "http://" or "https://", an HTTP proxy is used.
// Otherwise, the endpoint is treated as a gRPC address (host:port).
func New(endpoint string) AgentProxy {
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return &httpProxy{endpoint: strings.TrimRight(endpoint, "/")}
	}
	return &grpcProxy{endpoint: endpoint}
}
