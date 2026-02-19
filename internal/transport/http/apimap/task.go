package apimap

import (
	v1 "github.com/soltiHQ/control-plane/api/v1"
	"github.com/soltiHQ/control-plane/internal/proxy"
)

// TaskFromProxy converts a proxy.Task to an API v1.Task.
func TaskFromProxy(t proxy.Task) v1.Task {
	return v1.Task{
		ID:        t.ID,
		Slot:      t.Slot,
		Status:    t.Status,
		Attempt:   t.Attempt,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
		Error:     t.Error,
	}
}

// TasksFromProxy converts a slice of proxy.Task to a slice of v1.Task.
func TasksFromProxy(tasks []proxy.Task) []v1.Task {
	out := make([]v1.Task, len(tasks))
	for i, t := range tasks {
		out[i] = TaskFromProxy(t)
	}
	return out
}
