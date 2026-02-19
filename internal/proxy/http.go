package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const httpTimeout = 10 * time.Second

// httpProxy implements AgentProxy over HTTP.
type httpProxy struct {
	endpoint string
}

// httpTasksResponse mirrors the JSON shape returned by the agent's HTTP API.
type httpTasksResponse struct {
	Tasks []httpTask `json:"tasks"`
	Total int        `json:"total"`
}

type httpTask struct {
	ID        string `json:"id"`
	Slot      string `json:"slot"`
	Status    string `json:"status"`
	Attempt   int    `json:"attempt"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	Error     string `json:"error,omitempty"`
}

func (p *httpProxy) ListTasks(ctx context.Context, f TaskFilter) (*TaskListResult, error) {
	u, err := url.Parse(p.endpoint + "/api/v1/tasks")
	if err != nil {
		return nil, fmt.Errorf("proxy: bad endpoint URL: %w", err)
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

	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("proxy: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy: request to agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy: agent returned status %d", resp.StatusCode)
	}

	var body httpTasksResponse
	if err = json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("proxy: decode response: %w", err)
	}

	tasks := make([]Task, len(body.Tasks))
	for i, t := range body.Tasks {
		tasks[i] = Task{
			ID:        t.ID,
			Slot:      t.Slot,
			Status:    t.Status,
			Attempt:   t.Attempt,
			CreatedAt: t.CreatedAt,
			UpdatedAt: t.UpdatedAt,
			Error:     t.Error,
		}
	}

	return &TaskListResult{
		Tasks: tasks,
		Total: body.Total,
	}, nil
}
