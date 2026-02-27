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
	Limit  int
	Offset int
	Slot   string
	Status string
}

// TaskSubmission describes a task to push to an agent.
// The Spec field is the agent CreateSpec JSON (slot, kind, timeoutMs, restart, backoff, admission, labels).
type TaskSubmission struct {
	Spec map[string]any `json:"spec"`
}

// SpecExport describes a task spec as reported by an agent via export.
type SpecExport struct {
	Version int            `json:"version"`
	Slot    string         `json:"slot"`
	Kind    map[string]any `json:"kind,omitempty"`
}

// AgentProxy is the interface for outbound communication with an agent.
type AgentProxy interface {
	ListTasks(ctx context.Context, filter TaskFilter) (*proxyv1.TaskListResponse, error)
	SubmitTask(ctx context.Context, sub TaskSubmission) error
}
