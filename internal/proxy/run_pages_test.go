package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type runPageServer struct {
	taskv1.UnimplementedTaskServiceServer
	response *taskv1.ListTaskRunsResponse
	request  *taskv1.ListTaskRunsRequest
	failure  error
}

func (s *runPageServer) ListTaskRuns(_ context.Context, req *taskv1.ListTaskRunsRequest) (*taskv1.ListTaskRunsResponse, error) {
	s.request = req
	return s.response, s.failure
}

func pageGRPCProxy(t *testing.T, s *runPageServer) *grpcProxyV1 {
	t.Helper()
	l := &memoryListener{conns: make(chan net.Conn), done: make(chan struct{})}
	server := grpc.NewServer()
	taskv1.RegisterTaskServiceServer(server, s)
	go func() { _ = server.Serve(l) }()
	conn, err := grpc.NewClient("passthrough:///run-page", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); server.Stop(); l.Close() })
	return &grpcProxyV1{conn: conn}
}

func TestRunPageBothTransportsPreserveSnapshotGVKAndTimestampPresence(t *testing.T) {
	zero := int32(0)
	epoch := int64(0)
	response := &taskv1.ListTaskRunsResponse{TaskUid: "uid", ResourceVersion: "opaque-rv", Continue: "next", Runs: []*taskv1.TaskRunInfo{{Generation: 4, Attempt: 2, Phase: taskv1.TaskPhase_TASK_PHASE_SUCCEEDED, StartedAt: 10, FinishedAt: &epoch, ExitCode: &zero, WorkloadApiVersion: "example.io/v2", WorkloadKind: "CustomTask"}, {Generation: 5, Attempt: 1, Phase: taskv1.TaskPhase_TASK_PHASE_RUNNING}}}
	expected := &TaskRunPage{TaskUID: "uid", ResourceVersion: "opaque-rv", NextCursor: "next", Runs: []proxyv1.TaskRun{{Generation: 4, Attempt: 2, Status: "succeeded", StartedAt: 10, FinishedAt: 0, FinishedAtPresent: true, ExitCode: &zero, WorkloadAPIVersion: "example.io/v2", WorkloadKind: "CustomTask"}, {Generation: 5, Attempt: 1, Status: "running"}}}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("continue") != "current" {
			t.Error("run query pagination lost")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"taskUid": "uid", "resourceVersion": "opaque-rv", "continue": "next"}, "runs": []any{map[string]any{"generation": 4, "attempt": 2, "phase": "succeeded", "startedAt": time.UnixMilli(10).UTC().Format(time.RFC3339Nano), "finishedAt": time.UnixMilli(0).UTC().Format(time.RFC3339Nano), "exitCode": 0, "workload": map[string]string{"apiVersion": "example.io/v2", "kind": "CustomTask"}}, map[string]any{"generation": 5, "attempt": 1, "phase": "running"}}})
	}))
	defer server.Close()
	grpcServer := &runPageServer{response: response}
	transports := map[string]TaskRunPager{"http": &httpProxyV1{endpoint: server.URL, client: server.Client()}, "grpc": pageGRPCProxy(t, grpcServer)}
	for name, p := range transports {
		t.Run(name, func(t *testing.T) {
			page, err := p.ListTaskRunsPage(context.Background(), "task", TaskRunQuery{Limit: 2, Cursor: "current"})
			if err != nil || !reflect.DeepEqual(page, expected) {
				t.Fatalf("run metadata/presence lost page=%+v err=%v", page, err)
			}
		})
	}
	if calls != 1 || grpcServer.request.GetLimit() != 2 || grpcServer.request.GetContinue() != "current" || grpcServer.request.GetName() != "task" {
		t.Fatal("page API eagerly aggregated or omitted filters")
	}
}

func TestExpiredRunPagesHaveTransportIndependentSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":"ResourceVersionExpired","message":"old"}`))
	}))
	defer server.Close()
	transports := map[string]TaskRunPager{"http": &httpProxyV1{endpoint: server.URL, client: server.Client()}, "grpc": pageGRPCProxy(t, &runPageServer{failure: status.Error(codes.OutOfRange, "old")})}
	for name, p := range transports {
		t.Run(name, func(t *testing.T) {
			_, err := p.ListTaskRunsPage(context.Background(), "task", TaskRunQuery{Limit: 1, Cursor: "old"})
			if !errors.Is(err, ErrRunSnapshotExpired) || !errors.Is(err, ErrListTaskRuns) {
				t.Fatalf("expiry lost %v", err)
			}
		})
	}
}

func TestRunPageInvalidLimitFailsBeforeTransport(t *testing.T) {
	p := &httpProxyV1{}
	for _, limit := range []uint32{0, 1001} {
		if _, err := p.ListTaskRunsPage(context.Background(), "task", TaskRunQuery{Limit: limit}); !errors.Is(err, ErrListTaskRuns) {
			t.Fatal("invalid page limit sent to source")
		}
	}
}

func TestObservedTaskTimestampsNeverInventUpdateOrMissingCreation(t *testing.T) {
	flat := (taskResource{Metadata: resourceMeta{Name: "task", UID: "uid"}}).flat()
	if flat.CreatedAt != 0 || flat.UpdatedAt != 0 {
		t.Fatalf("missing source timestamp invented %+v", flat)
	}
	flat = (taskResource{Metadata: resourceMeta{Name: "task", UID: "uid", CreatedAt: time.UnixMilli(123)}}).flat()
	if flat.CreatedAt != 123 || flat.UpdatedAt != 0 {
		t.Fatal("creation aliased to unknown update")
	}
	grpcFlat := taskDataToProxy(&taskv1.Task{Metadata: &taskv1.ObjectMeta{Name: "task", Uid: "uid", CreationTimestamp: 123}})
	if grpcFlat.CreatedAt != 123 || grpcFlat.UpdatedAt != 0 {
		t.Fatal("gRPC creation aliased to unknown update")
	}
}
