package sync

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/proxy"
)

// floodConditions is how many empty conditions the task of a flooding agent holds:
// 2 MiB of protobuf and 3 MB of JSON, under the 4 MiB that the proxy reads.
const floodConditions = 1 << 20

// floodingAgent starts an agent of one transport that answers every ListTasks call
// with one task of floodConditions empty conditions, and returns its endpoint, the
// type of it as the proto enum has it (1 for gRPC, 2 for HTTP) and how many calls it
// has answered.
type floodingAgent func(t *testing.T) (endpoint string, endpointType int, calls *atomic.Int32)

func httpFlooding(t *testing.T) (string, int, *atomic.Int32) {
	t.Helper()
	body := `{"metadata":{},"items":[{"metadata":{"name":"task-flood","uid":"u"},"status":{"conditions":[` +
		strings.Repeat("{},", floodConditions-1) + `{}]}}]}`
	calls := new(atomic.Int32)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(agent.Close)
	return agent.URL, 2, calls
}

// listAgent is a gRPC agent whose ListTasks returns page, a marker that the codec of
// its server swaps for the bytes it holds.
type listAgent struct {
	taskv1.UnimplementedTaskServiceServer
	page  *taskv1.ListTasksResponse
	calls *atomic.Int32
}

func (a listAgent) ListTasks(context.Context, *taskv1.ListTasksRequest) (*taskv1.ListTasksResponse, error) {
	a.calls.Add(1)
	return a.page, nil
}

// swapCodec marshals the message marker as the bytes wire, as an agent that built its
// answer by hand would, and every other message as the proto codec does. The proto
// codec of a server cannot: the million conditions it would have to marshal cost the
// test the heap it is not about.
type swapCodec struct {
	encoding.CodecV2
	marker any
	wire   []byte
}

func (c swapCodec) Marshal(v any) (mem.BufferSlice, error) {
	if v == c.marker {
		return mem.BufferSlice{mem.SliceBuffer(c.wire)}, nil
	}
	return c.CodecV2.Marshal(v)
}

// entry is payload as the value of the field called name of a message of the type of
// msg, which the schema says is length-delimited.
func entry(msg proto.Message, name string, payload []byte) []byte {
	fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		panic("no field " + name + " in " + string(msg.ProtoReflect().Descriptor().FullName()))
	}
	return protowire.AppendBytes(protowire.AppendTag(nil, fd.Number(), protowire.BytesType), payload)
}

// floodPage is the wire bytes of a ListTasks answer with one task named "task-flood"
// whose status holds floodConditions empty conditions.
func floodPage() []byte {
	conditions := bytes.Repeat(entry(&taskv1.TaskStatus{}, "conditions", nil), floodConditions)
	meta := entry(&taskv1.ObjectMeta{}, "name", []byte("task-flood"))
	task := append(entry(&taskv1.Task{}, "metadata", meta), entry(&taskv1.Task{}, "status", conditions)...)
	return entry(&taskv1.ListTasksResponse{}, "tasks", task)
}

func grpcFlooding(t *testing.T) (string, int, *atomic.Int32) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int32)
	page := &taskv1.ListTasksResponse{}
	agent := grpc.NewServer(grpc.ForceServerCodecV2(swapCodec{CodecV2: encoding.GetCodecV2("proto"), marker: page, wire: floodPage()}))
	taskv1.RegisterTaskServiceServer(agent, listAgent{page: page, calls: calls})
	go func() { _ = agent.Serve(lis) }()
	t.Cleanup(agent.Stop)
	return lis.Addr().String(), 1, calls
}

// A task with a million conditions is the same flood over either transport: the
// proxy refuses it as a task with too many conditions, which no smaller page avoids,
// and the runner ends the listing at the call that meets it, with the cause of its
// own, without asking again with half the limit. The agents are real ones and the
// pool is the real one: what the runner gets is what the proxy makes of the bytes.
// Over gRPC the scan of the answer meets the 65th condition long before the budget
// of elements in all, which the runner asks again smaller for, up to ten times.
func TestATaskOfAMillionConditionsEndsTheListingAtOnceOverBothTransports(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent floodingAgent
	}{
		{"http", httpFlooding},
		{"grpc", grpcFlooding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, endpointType, calls := tc.agent(t)
			r, s, _ := newFakeRunner(t, &fakeProxy{})
			logs := new(bytes.Buffer)
			r.logger = zerolog.New(logs)
			pool := proxy.NewPool(nil, false)
			t.Cleanup(func() { _ = pool.Close() })
			r.pool = pool
			observed := seedRollout(t, s, "sp-observed", "agent-a", enum.RolloutIntentNoop, "task-observed")
			ag, err := model.NewAgentFrom(model.AgentParams{ID: "agent-a", Name: "agent-a", Endpoint: endpoint, EndpointType: endpointType, APIVersion: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.UpsertAgent(context.Background(), ag); err != nil {
				t.Fatal(err)
			}

			r.tick(context.Background())

			if got := calls.Load(); got != 1 {
				t.Fatalf("the agent answered %d list calls, want 1", got)
			}
			if got := r.incomplete["agent-a"]; got != causeConditions {
				t.Fatalf("the listing is incomplete for %q, want %q", got, causeConditions)
			}
			if !unchanged(t, s, observed) {
				t.Fatal("the observed rollout changed on an incomplete listing")
			}
			if b, ok := r.backoffs["agent-a"]; ok {
				t.Fatalf("the agent answered, but is backed off: %+v", b)
			}
			if warns := strings.Count(logs.String(), `"level":"warn"`); warns != 1 || !strings.Contains(logs.String(), causeConditions) {
				t.Fatalf("logged %d warnings, want one naming %q: %.300s", warns, causeConditions, logs.String())
			}
		})
	}
}
