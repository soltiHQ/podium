package proxy

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
)

const (
	v1PathTasks = "/api/v1/tasks"
)

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

	return doGet[proxyv1.TaskListResponse](ctx, p.client, u.String())
}

func (p *httpProxyV1) SubmitTask(ctx context.Context, sub TaskSubmission) error {
	u, err := url.Parse(p.endpoint + v1PathTasks)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadEndpointURL, err)
	}

	return doPost(ctx, p.client, u.String(), map[string]any{"spec": sub.Spec})
}
