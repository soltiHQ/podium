// Package proxy provides outbound communication with agents.
//
// The control-plane calls INTO agents to query tasks, submit work, etc.
// Each agent exposes either an HTTP or a gRPC endpoint; the proxy package
// selects the transport based on the endpoint type reported by the agent.
package proxy

import (
	"context"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
)

// TaskFilter holds optional filters and pagination for listing tasks.
type TaskFilter struct {
	Slot   string
	Status string
	Limit  int
	Offset int
}

// AgentProxy is the interface for outbound communication with an agent.
type AgentProxy interface {
	ListTasks(ctx context.Context, filter TaskFilter) (*proxyv1.TaskListResponse, error)
}
