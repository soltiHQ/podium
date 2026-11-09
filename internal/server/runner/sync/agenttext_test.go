package sync

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// statusAgent is a gRPC agent that fails every call with err.
type statusAgent struct {
	taskv1.UnimplementedTaskServiceServer
	err error
}

func (a statusAgent) ApplyTask(context.Context, *taskv1.ApplyTaskRequest) (*taskv1.ApplyTaskResponse, error) {
	return nil, a.err
}

func (a statusAgent) ListTasks(context.Context, *taskv1.ListTasksRequest) (*taskv1.ListTasksResponse, error) {
	return nil, a.err
}

// failingAgent starts an agent of one transport that fails every call with a text
// of size bytes, and returns its endpoint and the type of it, as the proto enum has
// it: 1 for gRPC, 2 for HTTP.
type failingAgent func(t *testing.T, size int) (endpoint string, endpointType int)

func httpFailing(t *testing.T, size int) (string, int) {
	t.Helper()
	body := strings.Repeat("x", size)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, body, http.StatusInternalServerError)
	}))
	t.Cleanup(agent.Close)
	return agent.URL, 2
}

func grpcFailing(t *testing.T, size int) (string, int) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	agent := grpc.NewServer()
	taskv1.RegisterTaskServiceServer(agent, statusAgent{err: status.Error(codes.Internal, strings.Repeat("x", size))})
	go func() { _ = agent.Serve(lis) }()
	t.Cleanup(agent.Stop)
	return lis.Addr().String(), 1
}

// An agent's error is text the agent chose, and a hostile agent chooses a lot of
// it: over HTTP as much as the body it sends, over gRPC as much as a header list
// holds. What the runner stores in the rollout, records in the event and logs is
// cut to proxy.MaxAgentTextBytes on a UTF-8 boundary, and ends with a marker where
// it was cut. A status over the header list a client accepts fails the call with
// an error of the transport, a short one.
func TestAgentErrorTextIsCapped(t *testing.T) {
	const limit = proxy.MaxAgentTextBytes + len("…")
	for _, tc := range []struct {
		name  string
		agent failingAgent
		size  int
		cut   bool // the text is cut: it reached the runner whole
	}{
		{"an HTTP error body of 1 MiB", httpFailing, 1 << 20, true},
		{"a gRPC status of 32 KiB", grpcFailing, 32 << 10, true},
		{"a gRPC status over 64 KiB", grpcFailing, 100 << 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, endpointType := tc.agent(t, tc.size)
			r, s, hub := newFakeRunner(t, &fakeProxy{})
			logs := new(bytes.Buffer)
			r.logger = zerolog.New(logs)
			pool := proxy.NewPool(nil, false)
			t.Cleanup(func() { _ = pool.Close() })
			r.pool = pool
			pushed := seedRollout(t, s, "sp-pushed", "agent-a", enum.RolloutIntentInstall, "")
			observed := seedRollout(t, s, "sp-observed", "agent-a", enum.RolloutIntentNoop, "task-observed")
			ag, err := model.NewAgentFrom(model.AgentParams{ID: "agent-a", Name: "agent-a", Endpoint: endpoint, EndpointType: endpointType, APIVersion: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.UpsertAgent(context.Background(), ag); err != nil {
				t.Fatal(err)
			}

			r.tick(context.Background())

			after := mustRollout(t, s, pushed.ID())
			if after.Status() != enum.SyncStatusFailed || !strings.Contains(after.Error(), "apply task") {
				t.Fatalf("the push is %s: %q, want it failed", after.Status(), after.Error())
			}
			if len(after.Error()) > limit {
				t.Fatalf("the rollout error is %d bytes, want at most %d", len(after.Error()), limit)
			}
			if tc.cut && !strings.HasSuffix(after.Error(), "…") {
				t.Fatalf("the rollout error does not end where it was cut: ...%q", after.Error()[len(after.Error())-8:])
			}
			if !tc.cut && len(after.Error()) > 1<<10 {
				t.Fatalf("a status over the header list cap left a rollout error of %d bytes, want a short one", len(after.Error()))
			}
			var detail string
			for _, ev := range hub.RecentEvents(event.RingSize) {
				if ev.Kind == event.SyncFailed {
					detail = ev.Payload.Detail
				}
			}
			if detail != after.Error() {
				t.Fatalf("the event detail is %d bytes and differs from the rollout error of %d", len(detail), len(after.Error()))
			}
			if _, listed := r.incomplete["agent-a"]; !listed {
				t.Fatal("the listing of the agent did not fail, so no log line carries its error")
			}
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				if len(line) > limit+512 {
					t.Fatalf("a log line of %d bytes carries the agent's text: %.200s...", len(line), line)
				}
			}
			if !unchanged(t, s, observed) {
				t.Fatalf("the observed rollout changed on an incomplete listing")
			}
		})
	}
}

func mustRollout(t *testing.T, s storage.Storage, id string) *model.Rollout {
	t.Helper()
	ro, err := s.GetRollout(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ro
}
