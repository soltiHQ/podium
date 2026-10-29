package proxy

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type conditionsServer struct {
	taskv1.UnimplementedTaskServiceServer
	task *taskv1.Task
}

func (s *conditionsServer) GetTask(context.Context, *taskv1.GetTaskRequest) (*taskv1.GetTaskResponse, error) {
	return &taskv1.GetTaskResponse{Task: s.task}, nil
}

func (s *conditionsServer) ListTasks(context.Context, *taskv1.ListTasksRequest) (*taskv1.ListTasksResponse, error) {
	return &taskv1.ListTasksResponse{Tasks: []*taskv1.Task{s.task}}, nil
}

func conditionsGRPCProxy(t *testing.T, resource *taskv1.Task) *grpcProxyV1 {
	t.Helper()
	l := &memoryListener{conns: make(chan net.Conn), done: make(chan struct{})}
	server := grpc.NewServer()
	taskv1.RegisterTaskServiceServer(server, &conditionsServer{task: resource})
	go func() { _ = server.Serve(l) }()
	conn, err := grpc.NewClient("passthrough:///conditions", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); server.Stop(); l.Close() })
	return &grpcProxyV1{conn: conn}
}

func TestGetAndListPreserveReconciliationConditionsBothTransports(t *testing.T) {
	const transition int64 = 1790812800123
	expected := []proxyv1.TaskCondition{
		{Type: "Reconciled", Status: "False", ObservedGeneration: 7, LastTransitionTime: int64Pointer(transition), Reason: "RunnerBuildFailed", Message: "runner unavailable — execution never started"},
		{Type: "example.io/Available", Status: "True", ObservedGeneration: 6, LastTransitionTime: int64Pointer(0), Reason: "Available", Message: "previous desired generation"},
		{Type: "example.io/Progressing", Status: "Unknown", ObservedGeneration: 7, LastTransitionTime: int64Pointer(transition), Reason: "Waiting", Message: ""},
	}
	resource := &taskv1.Task{Metadata: &taskv1.ObjectMeta{Name: "task", Uid: "incarnation", Generation: 7}, Spec: &taskv1.TaskSpec{Slot: "slot"}, Status: &taskv1.TaskStatus{Phase: taskv1.TaskPhase_TASK_PHASE_PENDING, ObservedGeneration: 7, Attempt: 0, Conditions: []*taskv1.TaskCondition{
		{Type: "Reconciled", Status: taskv1.ConditionStatus_CONDITION_STATUS_FALSE, ObservedGeneration: 7, LastTransitionTime: transition, Reason: "RunnerBuildFailed", Message: expected[0].Message},
		{Type: "example.io/Available", Status: taskv1.ConditionStatus_CONDITION_STATUS_TRUE, ObservedGeneration: 6, LastTransitionTime: 0, Reason: "Available", Message: expected[1].Message},
		{Type: "example.io/Progressing", Status: taskv1.ConditionStatus_CONDITION_STATUS_UNKNOWN, ObservedGeneration: 7, LastTransitionTime: transition, Reason: "Waiting", Message: ""},
	}}}
	// SDK reconciliation failure deliberately has pending/attempt0/no error even
	// after observedGeneration reaches desired. The diagnostic lives in conditions.
	httpConditions := []map[string]any{}
	for _, condition := range expected {
		httpConditions = append(httpConditions, map[string]any{"type": condition.Type, "status": condition.Status, "observedGeneration": condition.ObservedGeneration, "lastTransitionTime": time.UnixMilli(*condition.LastTransitionTime).UTC().Format(time.RFC3339Nano), "reason": condition.Reason, "message": condition.Message})
	}
	httpResource := map[string]any{"metadata": map[string]any{"name": "task", "uid": "incarnation", "generation": 7}, "spec": map[string]any{"slot": "slot"}, "status": map[string]any{"phase": "pending", "attempt": 0, "observedGeneration": 7, "conditions": httpConditions}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == v1PathTasks {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{httpResource}})
		} else {
			_ = json.NewEncoder(w).Encode(httpResource)
		}
	}))
	defer srv.Close()
	transports := map[string]AgentProxy{"http": &httpProxyV1{endpoint: srv.URL, client: srv.Client()}, "grpc": conditionsGRPCProxy(t, resource)}
	for name, p := range transports {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := p.GetTask(ctx, "task")
			if err != nil {
				t.Fatal(err)
			}
			page, err := p.ListTasks(ctx, TaskFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Tasks) != 1 {
				t.Fatal("missing listed task")
			}
			for _, task := range []proxyv1.Task{*got.Task, page.Tasks[0]} {
				if task.Status != "pending" || task.Attempt != 0 || task.Error != "" || task.ExitCode != nil || task.Generation != 7 || task.ObservedGeneration != 7 {
					t.Fatalf("condition changed execution/defaults: %+v", task)
				}
				if !reflect.DeepEqual(task.Conditions, expected) {
					t.Fatalf("conditions dropped/changed: %+v", task.Conditions)
				}
				raw, err := json.Marshal(task)
				if err != nil {
					t.Fatal(err)
				}
				var dto map[string]any
				if err = json.Unmarshal(raw, &dto); err != nil {
					t.Fatal(err)
				}
				cond := dto["conditions"].([]any)[0].(map[string]any)
				if cond["status"] != "False" || cond["message"] != expected[0].Message || cond["observed_generation"] != float64(7) || cond["last_transition_time"] != float64(transition) {
					t.Fatalf("REST conditions lost: %s", raw)
				}
			}
		})
	}
}
func int64Pointer(value int64) *int64 { return &value }

func TestConditionDefaultsDoNotInventTransitionsOrTrueStatus(t *testing.T) {
	var resource taskResource
	if err := json.Unmarshal([]byte(`{"status":{"conditions":[{"type":"Reconciled","status":"Unknown","observedGeneration":0,"lastTransitionTime":null,"reason":"","message":""}]}}`), &resource); err != nil {
		t.Fatal(err)
	}
	got := resource.flat().Conditions[0]
	if got.LastTransitionTime != nil || got.Status != "Unknown" || got.ObservedGeneration != 0 || got.Message != "" || got.Reason != "" {
		t.Fatalf("HTTP default changed: %+v", got)
	}
	protoTask := taskDataToProxy(&taskv1.Task{Status: &taskv1.TaskStatus{Conditions: []*taskv1.TaskCondition{{}}}})
	got = protoTask.Conditions[0]
	if got.Status != "" || got.LastTransitionTime == nil || *got.LastTransitionTime != 0 || got.ObservedGeneration != 0 {
		t.Fatalf("proto default changed: %+v", got)
	}
	empty := taskDataToProxy(&taskv1.Task{})
	if empty.Conditions == nil || len(empty.Conditions) != 0 || empty.ExitCode != nil {
		t.Fatalf("empty protobuf status changed: %+v", empty)
	}
}
