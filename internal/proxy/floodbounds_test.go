package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

// million is how many empty elements a flood holds: 2 MiB of protobuf, 3 MB of JSON.
const million = 1 << 20

// answers are the bytes a rawAnswers agent answers each call with.
type answers struct{ list, get, apply, runs []byte }

// rawAnswers is a gRPC agent that answers ListTasks, GetTask, ApplyTask and
// ListTaskRuns with bytes it holds, as an agent that built its answers by hand
// would. The proto codec of a server cannot: the millions of elements it would have
// to marshal cost the test the heap it measures.
type rawAnswers struct {
	taskv1.UnimplementedTaskServiceServer
	// What each call returns, for the codec to swap for the bytes held under it.
	list  *taskv1.ListTasksResponse
	get   *taskv1.GetTaskResponse
	apply *taskv1.ApplyTaskResponse
	runs  *taskv1.ListTaskRunsResponse
	wire  map[any][]byte
}

// newRawAnswers makes an agent that answers each call with the bytes given for it.
func newRawAnswers(a answers) *rawAnswers {
	r := &rawAnswers{list: &taskv1.ListTasksResponse{}, get: &taskv1.GetTaskResponse{}, apply: &taskv1.ApplyTaskResponse{}, runs: &taskv1.ListTaskRunsResponse{}}
	r.wire = map[any][]byte{r.list: a.list, r.get: a.get, r.apply: a.apply, r.runs: a.runs}
	return r
}

func (a *rawAnswers) ListTasks(context.Context, *taskv1.ListTasksRequest) (*taskv1.ListTasksResponse, error) {
	return a.list, nil
}

func (a *rawAnswers) GetTask(context.Context, *taskv1.GetTaskRequest) (*taskv1.GetTaskResponse, error) {
	return a.get, nil
}

func (a *rawAnswers) ApplyTask(context.Context, *taskv1.ApplyTaskRequest) (*taskv1.ApplyTaskResponse, error) {
	return a.apply, nil
}

func (a *rawAnswers) ListTaskRuns(context.Context, *taskv1.ListTaskRunsRequest) (*taskv1.ListTaskRunsResponse, error) {
	return a.runs, nil
}

// rawCodec marshals the marker answers of a rawAnswers as the bytes held for them,
// and every other message as the proto codec does.
type rawCodec struct {
	encoding.CodecV2
	wire map[any][]byte
}

func (c rawCodec) Marshal(v any) (mem.BufferSlice, error) {
	if b, ok := c.wire[v]; ok {
		return mem.BufferSlice{mem.SliceBuffer(b)}, nil
	}
	return c.CodecV2.Marshal(v)
}

// proxy is a gRPC client of the agent.
func (a *rawAnswers) proxy(t *testing.T) *grpcProxyV1 {
	t.Helper()
	codec := rawCodec{CodecV2: encoding.GetCodecV2("proto"), wire: a.wire}
	return grpcContractProxyOver(t, a, []grpc.ServerOption{grpc.ForceServerCodecV2(codec)})
}

// path resolves the names of nested fields, from the message msg on, to field
// numbers; every name but the last is a message field.
func path(msg proto.Message, names ...string) []protowire.Number {
	md := msg.ProtoReflect().Descriptor()
	out := make([]protowire.Number, len(names))
	for i, name := range names {
		fd := md.Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			panic(fmt.Sprintf("%s has no field %q", md.FullName(), name))
		}
		out[i] = fd.Number()
		md = fd.Message()
	}
	return out
}

// field is payload as the value of the length-delimited field num.
func field(num protowire.Number, payload []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), payload)
}

// within wraps payload in the fields nums, the first outermost.
func within(nums []protowire.Number, payload []byte) []byte {
	for i := len(nums) - 1; i >= 0; i-- {
		payload = field(nums[i], payload)
	}
	return payload
}

// emptyEntries is n empty entries of the length-delimited field num: two bytes each.
func emptyEntries(num protowire.Number, n int) []byte {
	return bytes.Repeat(field(num, nil), n)
}

// flood is the bytes of a message of type msg that holds n empty entries of the
// repeated field the last of names selects, inside the singular message fields the
// names before it select.
func flood(msg proto.Message, n int, names ...string) []byte {
	nums := path(msg, names...)
	return within(nums[:len(nums)-1], emptyEntries(nums[len(nums)-1], n))
}

// jsonEmpties is n empty JSON objects, comma separated.
func jsonEmpties(n int) string {
	return strings.Repeat("{},", n-1) + "{}"
}

// serveJSON serves body as the answer to every call, and returns the client of it.
func serveJSON(t *testing.T, body string) *httpProxyV1 {
	t.Helper()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(agent.Close)
	return &httpProxyV1{endpoint: agent.URL, client: agent.Client()}
}

// wantTooManyElements fails unless err is an answer of the call op refused for the
// elements it holds in all: a decode error that is an ErrTooManyElements, and so an
// ErrTooManyItems. It is not a task with too many conditions, which no smaller page
// avoids, nor one the proxy refused for its size, and errkind sorts it as the error
// of an agent.
func wantTooManyElements(t *testing.T, err, op error) {
	t.Helper()
	if !errors.Is(err, op) || !errors.Is(err, ErrDecode) || !errors.Is(err, ErrTooManyElements) || !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("%v: want a %v decode error that is ErrTooManyElements", err, op)
	}
	if errors.Is(err, ErrTooManyConditions) {
		t.Fatalf("%v: a budget of elements is no task with too many conditions: only the first is avoided by a smaller page", err)
	}
	if errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("%v: a flood of elements is no answer too large: the runner has a path of its own for each", err)
	}
	wantAgentError(t, err)
}

// wantTooManyConditions fails unless err is an answer of the call op refused for a
// task with more conditions than MaxTaskConditions: a decode error that is an
// ErrTooManyConditions, and so an ErrTooManyItems. It is not a budget of elements,
// which a smaller page may come under, nor an answer too large.
func wantTooManyConditions(t *testing.T, err, op error) {
	t.Helper()
	if !errors.Is(err, op) || !errors.Is(err, ErrDecode) || !errors.Is(err, ErrTooManyConditions) || !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("%v: want a %v decode error that is ErrTooManyConditions", err, op)
	}
	if errors.Is(err, ErrTooManyElements) {
		t.Fatalf("%v: a task with too many conditions is no budget of elements: a smaller page does not avoid it", err)
	}
	if errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("%v: a task with too many conditions is no answer too large", err)
	}
	wantAgentError(t, err)
}

// wantAgentError fails unless errkind sorts err as the error of an agent, and the
// error is short.
func wantAgentError(t *testing.T, err error) {
	t.Helper()
	if kind := errkind.Classify(err); kind != errkind.Internal {
		t.Fatalf("kind = %v, want Internal", kind)
	}
	if len(err.Error()) > 1<<10 {
		t.Fatalf("the error is %d bytes long", len(err.Error()))
	}
}

// A task with too many conditions and a budget of elements are two refusals, and
// neither is the other: the sync runner asks again with half the limit for the budget,
// which a smaller page may come under, and ends the listing for the task, which no
// smaller page avoids.
func TestTooManyConditionsIsNotTheBudgetOfElements(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		target error
		want   bool
	}{
		{"conditions are items", ErrTooManyConditions, ErrTooManyItems, true},
		{"conditions are a decode error", ErrTooManyConditions, ErrDecode, true},
		{"conditions are not the budget", ErrTooManyConditions, ErrTooManyElements, false},
		{"conditions are no answer too large", ErrTooManyConditions, ErrResponseTooLarge, false},
		{"the budget is items", ErrTooManyElements, ErrTooManyItems, true},
		{"the budget is not conditions", ErrTooManyElements, ErrTooManyConditions, false},
		{"the budget is no answer too large", ErrTooManyElements, ErrResponseTooLarge, false},
		{"the error for conditions is conditions", tooManyConditions(), ErrTooManyConditions, true},
		{"the error for conditions is items", tooManyConditions(), ErrTooManyItems, true},
		{"the error for conditions is a decode error", tooManyConditions(), ErrDecode, true},
		{"the error for conditions is not the budget", tooManyConditions(), ErrTooManyElements, false},
		{"the error for the budget is the budget", tooManyElements("more"), ErrTooManyElements, true},
		{"the error for the budget is not conditions", tooManyElements("more"), ErrTooManyConditions, false},
		{"a page over its limit is neither", tooManyItems(10), ErrTooManyConditions, false},
		{"a page over its limit is not the budget", tooManyItems(10), ErrTooManyElements, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errors.Is(tc.err, tc.target); got != tc.want {
				t.Fatalf("errors.Is(%v, %v) = %v, want %v", tc.err, tc.target, got, tc.want)
			}
		})
	}
}

// checkAllocated logs what a call allocated and fails when it is over the budget.
func checkAllocated(t *testing.T, grew uint64) {
	t.Helper()
	t.Logf("the call allocated %d MiB", grew>>20)
	if grew > pageBudget {
		t.Errorf("the call allocated %d MiB, want under %d MiB", grew>>20, pageBudget>>20)
	}
}

// A task is one item of a page, but it holds lists of its own: its conditions; the
// args and env of its workload and the rules of its selector in its spec; the
// labels of its metadata. An empty element takes two bytes on the wire and about a
// hundred on the heap, so one task can hold a million of them, and a page of a
// thousand tasks a thousand each. The proxy scans the wire bytes of every answer
// before it decodes them, and counts every element of every repeated field and map
// in the tree.
func TestGRPCListTasksRefusesNestedElementFloodBeforeDecode(t *testing.T) {
	oneTaskOfConditions := func(n int) []byte {
		return flood(&taskv1.ListTasksResponse{}, n, "tasks", "status", "conditions")
	}
	for _, tc := range []struct {
		name string
		page []byte
	}{
		{"a task with a million empty conditions", oneTaskOfConditions(million)},
		{"a task with a million empty env entries in its spec", flood(&taskv1.ListTasksResponse{}, million, "tasks", "spec", "workload", "subprocess", "env")},
		{"a task with a million empty args in its spec", flood(&taskv1.ListTasksResponse{}, million, "tasks", "spec", "workload", "wasm", "args")},
		{"a task with a million labels", flood(&taskv1.ListTasksResponse{}, million, "tasks", "metadata", "labels")},
		{"a thousand tasks with a thousand empty conditions each", bytes.Repeat(oneTaskOfConditions(1000), 1000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawAnswers(answers{list: tc.page}).proxy(t)

			var err error
			grew := allocated(func() { _, err = p.ListTasks(context.Background(), TaskFilter{Limit: 1000}) })

			checkAllocated(t, grew)
			wantTooManyElements(t, err, ErrListTasks)
		})
	}
}

// taskJSON is the JSON of a task named "task" with n conditions.
func taskJSON(n int) string {
	conditions := make([]string, n)
	for i := range conditions {
		conditions[i] = `{"type":"Ready","status":"True"}`
	}
	return `{"metadata":{"name":"task","uid":"u"},"status":{"conditions":[` + strings.Join(conditions, ",") + `]}}`
}

// The conditions of a task are decoded one at a time, and the one past
// MaxTaskConditions is refused before it is decoded, so a task of a million costs
// what a task of 65 does.
func TestHTTPListTasksRefusesConditionFlood(t *testing.T) {
	for _, tc := range []struct {
		name string
		task string
		ok   bool
	}{
		{"a million empty conditions", `{"metadata":{"name":"task","uid":"u"},"status":{"conditions":[` + jsonEmpties(million) + `]}}`, false},
		{"as many conditions as a task may hold", taskJSON(MaxTaskConditions), true},
		{"one condition more", taskJSON(MaxTaskConditions + 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := serveJSON(t, `{"metadata":{},"items":[`+tc.task+`]}`)

			var (
				conditions int
				err        error
			)
			grew := allocated(func() {
				resp, e := p.ListTasks(context.Background(), TaskFilter{Limit: 1000})
				if err = e; e == nil {
					conditions = len(resp.Tasks[0].Conditions)
				}
			})

			checkAllocated(t, grew)
			if tc.ok {
				if err != nil || conditions != MaxTaskConditions {
					t.Fatalf("ListTasks: %d conditions, %v, want %d", conditions, err, MaxTaskConditions)
				}
				return
			}
			wantTooManyConditions(t, err, ErrListTasks)
		})
	}
}

// taskServer is a gRPC agent with one task.
type taskServer struct {
	taskv1.UnimplementedTaskServiceServer
	task *taskv1.Task
}

func (s *taskServer) GetTask(context.Context, *taskv1.GetTaskRequest) (*taskv1.GetTaskResponse, error) {
	return &taskv1.GetTaskResponse{Task: s.task}, nil
}

func (s *taskServer) ListTasks(context.Context, *taskv1.ListTasksRequest) (*taskv1.ListTasksResponse, error) {
	return &taskv1.ListTasksResponse{Tasks: []*taskv1.Task{s.task}}, nil
}

func (s *taskServer) ApplyTask(context.Context, *taskv1.ApplyTaskRequest) (*taskv1.ApplyTaskResponse, error) {
	return &taskv1.ApplyTaskResponse{Task: s.task}, nil
}

// taskAgents are agents of both transports that hold a task named "task" with n conditions.
var taskAgents = []struct {
	name  string
	agent func(t *testing.T, conditions int) AgentProxy
}{
	{"http", func(t *testing.T, n int) AgentProxy {
		t.Helper()
		agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/tasks") {
				_, _ = io.WriteString(w, `{"metadata":{},"items":[`+taskJSON(n)+`]}`)
				return
			}
			_, _ = io.WriteString(w, taskJSON(n))
		}))
		t.Cleanup(agent.Close)
		return &httpProxyV1{endpoint: agent.URL, client: agent.Client()}
	}},
	{"grpc", func(t *testing.T, n int) AgentProxy {
		t.Helper()
		status := &taskv1.TaskStatus{}
		for range n {
			status.Conditions = append(status.Conditions, &taskv1.TaskCondition{Type: "Ready", Status: taskv1.ConditionStatus_CONDITION_STATUS_TRUE})
		}
		task := &taskv1.Task{Metadata: &taskv1.ObjectMeta{Name: "task", Uid: "u"}, Status: status}
		return grpcContractProxyOver(t, &taskServer{task: task}, nil)
	}},
}

// Both transports refuse the same tasks, with the same error: one with more
// conditions than MaxTaskConditions, in a listing, in GetTask and in the answer of
// ApplyTask. Over gRPC it passes the scan, which counts 640 conditions well under the
// budget, and is refused after it is decoded.
func TestTaskConditionsBound(t *testing.T) {
	for _, agent := range taskAgents {
		for _, n := range []int{0, 1, MaxTaskConditions, MaxTaskConditions + 1, 10 * MaxTaskConditions} {
			t.Run(fmt.Sprintf("%s/%d conditions", agent.name, n), func(t *testing.T) {
				p := agent.agent(t, n)
				ctx := context.Background()

				listed, listErr := p.ListTasks(ctx, TaskFilter{Limit: 10})
				got, getErr := p.GetTask(ctx, "task")
				applied, applyErr := p.ApplyTask(ctx, TaskSubmission{Spec: httpTestManifest(t, "task")})

				if n <= MaxTaskConditions {
					if listErr != nil || getErr != nil || applyErr != nil {
						t.Fatalf("list: %v, get: %v, apply: %v, want none", listErr, getErr, applyErr)
					}
					if len(listed.Tasks) != 1 || len(listed.Tasks[0].Conditions) != n || len(got.Task.Conditions) != n || applied != "task" {
						t.Fatalf("listed %+v, got %+v, applied %q, want a task with %d conditions", listed, got, applied, n)
					}
					return
				}
				wantTooManyConditions(t, listErr, ErrListTasks)
				wantTooManyConditions(t, getErr, ErrGetTask)
				wantTooManyConditions(t, applyErr, ErrApplyTask)
			})
		}
	}
}

// The answer of GetTask is scanned as a page is: a task of a million empty
// conditions, or of a million empty entries elsewhere in its spec, is refused.
func TestGetTaskRefusesNestedElementFlood(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent func(t *testing.T) AgentProxy
		want  func(t *testing.T, err, op error) // the refusal: HTTP has the bound on conditions only
	}{
		{"http/a million empty conditions", func(t *testing.T) AgentProxy {
			return serveJSON(t, `{"metadata":{"name":"task","uid":"u"},"status":{"conditions":[`+jsonEmpties(million)+`]}}`)
		}, wantTooManyConditions},
		{"grpc/a million empty conditions", func(t *testing.T) AgentProxy {
			return newRawAnswers(answers{get: flood(&taskv1.GetTaskResponse{}, million, "task", "status", "conditions")}).proxy(t)
		}, wantTooManyElements},
		{"grpc/a million empty env entries", func(t *testing.T) AgentProxy {
			return newRawAnswers(answers{get: flood(&taskv1.GetTaskResponse{}, million, "task", "spec", "workload", "container", "env")}).proxy(t)
		}, wantTooManyElements},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.agent(t)

			var err error
			grew := allocated(func() { _, err = p.GetTask(context.Background(), "task") })

			checkAllocated(t, grew)
			tc.want(t, err, ErrGetTask)
		})
	}
}

// The answer of ApplyTask is scanned too, over gRPC, and its conditions are bounded
// over HTTP: neither builds a flood for the identity the proxy reads from it.
func TestApplyTaskRefusesNestedElementFlood(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent func(t *testing.T) AgentProxy
		want  func(t *testing.T, err, op error)
	}{
		{"http", func(t *testing.T) AgentProxy {
			return serveJSON(t, `{"metadata":{"name":"task","uid":"u"},"status":{"conditions":[`+jsonEmpties(million)+`]}}`)
		}, wantTooManyConditions},
		{"grpc", func(t *testing.T) AgentProxy {
			return newRawAnswers(answers{apply: flood(&taskv1.ApplyTaskResponse{}, million, "task", "status", "conditions")}).proxy(t)
		}, wantTooManyElements},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.agent(t)

			var err error
			grew := allocated(func() {
				_, err = p.ApplyTask(context.Background(), TaskSubmission{Spec: httpTestManifest(t, "task")})
			})

			checkAllocated(t, grew)
			tc.want(t, err, ErrApplyTask)
		})
	}
}

// runsServer is a gRPC agent with a page of runs.
type runsServer struct {
	taskv1.UnimplementedTaskServiceServer
	runs int
}

func (s *runsServer) ListTaskRuns(context.Context, *taskv1.ListTaskRunsRequest) (*taskv1.ListTaskRunsResponse, error) {
	resp := &taskv1.ListTaskRunsResponse{TaskUid: "u", ResourceVersion: "rv"}
	for range s.runs {
		resp.Runs = append(resp.Runs, &taskv1.TaskRunInfo{Generation: 1, Attempt: 1, Phase: taskv1.TaskPhase_TASK_PHASE_SUCCEEDED})
	}
	return resp, nil
}

// runsJSON is the JSON of a page of n runs.
func runsJSON(n int) string {
	runs := make([]string, n)
	for i := range runs {
		runs[i] = `{"generation":1,"attempt":1,"phase":"succeeded"}`
	}
	return `{"metadata":{"taskUid":"u","resourceVersion":"rv"},"runs":[` + strings.Join(runs, ",") + `]}`
}

// A page of runs is bounded as a page of tasks is: by the page size the request
// names, before the runs are built. The history inspector and the history archive
// ask for a page of up to 1000.
func TestListTaskRunsRefusesExtraRunsBeforeDecode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent func(t *testing.T) TaskRunPager
	}{
		{"http", func(t *testing.T) TaskRunPager {
			return serveJSON(t, `{"metadata":{"taskUid":"u","resourceVersion":"rv"},"runs":[`+jsonEmpties(1<<20)+`]}`)
		}},
		{"grpc", func(t *testing.T) TaskRunPager {
			return newRawAnswers(answers{runs: emptyEntries(runsField, maxAgentMessageBytes/2)}).proxy(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.agent(t)

			var err error
			grew := allocated(func() { _, err = p.ListTaskRunsPage(context.Background(), "task", TaskRunQuery{Limit: 1000}) })

			checkAllocated(t, grew)
			wantTooManyItems(t, err, ErrListTaskRuns)
			if _, err = aggregateTaskRuns(context.Background(), p, "task"); !errors.Is(err, ErrTooManyItems) {
				t.Fatalf("the live history of the task = %v, want the page refused", err)
			}
		})
	}
}

// A page of runs may hold as many as the page size says, and no more.
func TestListTaskRunsPageBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit uint32
		runs  int
		ok    bool
	}{
		{"as many as the page size", 2, 2, true},
		{"fewer", 2, 1, true},
		{"none", 2, 0, true},
		{"one more", 2, 3, false},
		{"as many as the most any page size allows", 1000, 1000, true},
		{"one more than the most", 1000, 1001, false},
	} {
		for _, agent := range []struct {
			name  string
			pager func(t *testing.T, runs int) TaskRunPager
		}{
			{"http", func(t *testing.T, runs int) TaskRunPager { return serveJSON(t, runsJSON(runs)) }},
			{"grpc", func(t *testing.T, runs int) TaskRunPager {
				return grpcContractProxyOver(t, &runsServer{runs: runs}, nil)
			}},
		} {
			t.Run(agent.name+"/"+tc.name, func(t *testing.T) {
				p := agent.pager(t, tc.runs)

				page, err := p.ListTaskRunsPage(context.Background(), "task", TaskRunQuery{Limit: tc.limit})

				if !tc.ok {
					wantTooManyItems(t, err, ErrListTaskRuns)
					return
				}
				if err != nil || len(page.Runs) != tc.runs || page.TaskUID != "u" {
					t.Fatalf("ListTaskRunsPage = %+v, %v, want a page of %d runs", page, err, tc.runs)
				}
			})
		}
	}
}

// A next cursor may be MaxCursorBytes long, and a page that names a longer one is
// refused as one that could not be decoded.
func TestListTasksRefusesLongCursor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cursor string
		ok     bool
	}{
		{"no cursor", "", true},
		{"a cursor of the longest length", strings.Repeat("c", MaxCursorBytes), true},
		{"a cursor one byte longer", strings.Repeat("c", MaxCursorBytes+1), false},
	} {
		for _, l := range listings {
			t.Run(l.name+"/"+tc.name, func(t *testing.T) {
				p := l.agent(t, []string{"task-0000"}, tc.cursor)

				resp, err := p.ListTasks(context.Background(), TaskFilter{Limit: 10})

				if tc.ok {
					if err != nil || resp.NextCursor != tc.cursor || len(resp.Tasks) != 1 {
						t.Fatalf("ListTasks = %+v, %v, want the page and its cursor", resp, err)
					}
					return
				}
				if !errors.Is(err, ErrListTasks) || !errors.Is(err, ErrDecode) || !errors.Is(err, ErrCursorTooLong) {
					t.Fatalf("ListTasks = %v, want a list tasks decode error that is ErrCursorTooLong", err)
				}
				if errors.Is(err, ErrTooManyItems) || errors.Is(err, ErrResponseTooLarge) {
					t.Fatalf("ListTasks = %v: a long cursor is neither too many items nor a page too large", err)
				}
				if kind := errkind.Classify(err); kind != errkind.Internal {
					t.Fatalf("kind = %v, want Internal", kind)
				}
				if len(err.Error()) > 1<<10 {
					t.Fatalf("the error is %d bytes long: does it hold the cursor?", len(err.Error()))
				}
			})
		}
	}
}

// The budget refuses what no agent sends and nothing else. A page of a thousand
// tasks, each at the cap of conditions with a hundred and fifty env entries, is
// 215000 elements, over four fifths of the budget, and decodes; so does an answer
// of exactly the budget, at the cost the tests measure, and one element more is
// refused.
func TestCompliantAnswersNearTheBudgetStillDecode(t *testing.T) {
	t.Run("grpc/a page of a thousand full tasks", func(t *testing.T) {
		task := &taskv1.Task{Metadata: &taskv1.ObjectMeta{Name: "task", Uid: "u"}, Status: &taskv1.TaskStatus{}, Spec: &taskv1.TaskSpec{Workload: &taskv1.TaskWorkload{
			Spec: &taskv1.TaskWorkload_Subprocess{Subprocess: &taskv1.SubprocessTask{}},
		}}}
		for range MaxTaskConditions {
			task.Status.Conditions = append(task.Status.Conditions, &taskv1.TaskCondition{Type: "T", Status: taskv1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "r"})
		}
		for range 150 {
			task.Spec.Workload.GetSubprocess().Env = append(task.Spec.Workload.GetSubprocess().Env, &taskv1.KeyValue{Key: "k", Value: "v"})
		}
		// A thousand entries of the tasks field: the bytes of one, a thousand times.
		wire := bytes.Repeat(marshal(t, &taskv1.ListTasksResponse{Tasks: []*taskv1.Task{task}}), 1000)
		if len(wire) > maxAgentMessageBytes {
			t.Fatalf("the page is %d bytes, over the %d the client reads", len(wire), maxAgentMessageBytes)
		}
		t.Logf("the page is %d bytes of 215000 elements", len(wire))
		p := newRawAnswers(answers{list: wire}).proxy(t)

		resp, err := p.ListTasks(context.Background(), TaskFilter{Limit: 1000})

		if err != nil || len(resp.Tasks) != 1000 || len(resp.Tasks[999].Conditions) != MaxTaskConditions {
			t.Fatalf("ListTasks: %d tasks, %v, want 1000 tasks of %d conditions", len(resp.Tasks), err, MaxTaskConditions)
		}
	})
	t.Run("http/a page of a thousand tasks at the cap of conditions", func(t *testing.T) {
		tasks := make([]string, 1000)
		for i := range tasks {
			tasks[i] = taskJSON(MaxTaskConditions)
		}
		p := serveJSON(t, `{"metadata":{},"items":[`+strings.Join(tasks, ",")+`]}`)

		resp, err := p.ListTasks(context.Background(), TaskFilter{Limit: 1000})

		if err != nil || len(resp.Tasks) != 1000 || len(resp.Tasks[999].Conditions) != MaxTaskConditions {
			t.Fatalf("ListTasks = %v, %v, want 1000 tasks of %d conditions", resp, err, MaxTaskConditions)
		}
	})
	meta, err := proto.Marshal(&taskv1.ObjectMeta{Name: "task", Uid: "u"})
	if err != nil {
		t.Fatal(err)
	}
	// A task of n empty env entries in its spec, with an identity.
	taskOfEnv := func(n int) []byte {
		task := path(&taskv1.GetTaskResponse{}, "task")
		inner := append(field(path(&taskv1.Task{}, "metadata")[0], meta), flood(&taskv1.Task{}, n, "spec", "workload", "subprocess", "env")...)
		return within(task, inner)
	}
	t.Run("grpc/an answer of exactly the budget", func(t *testing.T) {
		p := newRawAnswers(answers{get: taskOfEnv(maxAnswerElements)}).proxy(t)

		var err error
		grew := allocated(func() { _, err = p.GetTask(context.Background(), "task") })

		checkAllocated(t, grew)
		if err != nil {
			t.Fatalf("GetTask = %v, want the answer of %d elements", err, maxAnswerElements)
		}
	})
	t.Run("grpc/an answer one element over the budget", func(t *testing.T) {
		p := newRawAnswers(answers{get: taskOfEnv(maxAnswerElements + 1)}).proxy(t)

		_, err := p.GetTask(context.Background(), "task")

		wantTooManyElements(t, err, ErrGetTask)
	})
}

// Every unary call is made with the codec that scans the answer: ListTasks, GetTask,
// ApplyTask, DeleteTask, CancelTask and ListTaskRuns. DeleteTask and CancelTask
// resolve the task by a GetTask first.
func TestEveryUnaryCallIsScanned(t *testing.T) {
	var calls []string
	intercept := grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		for _, opt := range opts {
			if forced, ok := opt.(grpc.ForceCodecV2CallOption); ok {
				if _, scans := forced.CodecV2.(*answerCodec); scans {
					calls = append(calls, method)
				}
			}
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	})
	p := grpcContractProxyOver(t, &everyCallServer{}, nil, intercept)
	ctx := context.Background()
	submission := TaskSubmission{Spec: httpTestManifest(t, "task")}

	if _, err := p.ListTasks(ctx, TaskFilter{Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetTask(ctx, "task"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ApplyTask(ctx, submission); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteTask(ctx, "task"); err != nil {
		t.Fatal(err)
	}
	if err := p.CancelTask(ctx, "task"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListTaskRunsPage(ctx, "task", TaskRunQuery{Limit: 10}); err != nil {
		t.Fatal(err)
	}

	want := map[string]int{
		taskv1.TaskService_ListTasks_FullMethodName:    1,
		taskv1.TaskService_GetTask_FullMethodName:      3, // by itself, by DeleteTask and by CancelTask
		taskv1.TaskService_ApplyTask_FullMethodName:    1,
		taskv1.TaskService_DeleteTask_FullMethodName:   1,
		taskv1.TaskService_CancelTask_FullMethodName:   1,
		taskv1.TaskService_ListTaskRuns_FullMethodName: 1,
	}
	got := map[string]int{}
	for _, method := range calls {
		got[method]++
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls made with the scanning codec = %v, want %v", got, want)
	}
}

// everyCallServer answers each unary call with something small and valid.
type everyCallServer struct {
	taskv1.UnimplementedTaskServiceServer
}

func (*everyCallServer) task() *taskv1.Task {
	return &taskv1.Task{Metadata: &taskv1.ObjectMeta{Name: "task", Uid: "u"}}
}

func (s *everyCallServer) ListTasks(context.Context, *taskv1.ListTasksRequest) (*taskv1.ListTasksResponse, error) {
	return &taskv1.ListTasksResponse{Tasks: []*taskv1.Task{s.task()}}, nil
}

func (s *everyCallServer) GetTask(context.Context, *taskv1.GetTaskRequest) (*taskv1.GetTaskResponse, error) {
	return &taskv1.GetTaskResponse{Task: s.task()}, nil
}

func (s *everyCallServer) ApplyTask(context.Context, *taskv1.ApplyTaskRequest) (*taskv1.ApplyTaskResponse, error) {
	return &taskv1.ApplyTaskResponse{Task: s.task()}, nil
}

func (*everyCallServer) DeleteTask(context.Context, *taskv1.DeleteTaskRequest) (*taskv1.DeleteTaskResponse, error) {
	return &taskv1.DeleteTaskResponse{}, nil
}

func (*everyCallServer) CancelTask(context.Context, *taskv1.CancelTaskRequest) (*taskv1.CancelTaskResponse, error) {
	return &taskv1.CancelTaskResponse{}, nil
}

func (*everyCallServer) ListTaskRuns(context.Context, *taskv1.ListTaskRunsRequest) (*taskv1.ListTaskRunsResponse, error) {
	return &taskv1.ListTaskRunsResponse{TaskUid: "u", ResourceVersion: "rv"}, nil
}

func TestPageFieldsAreThePageFieldsOfTheSchema(t *testing.T) {
	if got := path(&taskv1.ListTasksResponse{}, "tasks")[0]; got != tasksField {
		t.Fatalf("tasks field = %d, want %d", got, tasksField)
	}
	if got := path(&taskv1.ListTaskRunsResponse{}, "runs")[0]; got != runsField {
		t.Fatalf("runs field = %d, want %d", got, runsField)
	}
}

// marshal is the wire bytes of m.
func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// scanElements walks the wire bytes of an answer against its message descriptor and
// counts every element of every repeated field in the tree.
func TestScanElements(t *testing.T) {
	var (
		getResponse  = (&taskv1.GetTaskResponse{}).ProtoReflect().Descriptor()
		listResponse = (&taskv1.ListTasksResponse{}).ProtoReflect().Descriptor()
		listRequest  = (&taskv1.ListTasksRequest{}).ProtoReflect().Descriptor()
		// A task of n conditions, as the answer of GetTask.
		taskOf = func(n int) []byte { return flood(&taskv1.GetTaskResponse{}, n, "task", "status", "conditions") }
		// A page of n tasks of withConditions conditions each.
		tasks = func(n, withConditions int) []byte {
			return bytes.Repeat(flood(&taskv1.ListTasksResponse{}, withConditions, "tasks", "status", "conditions"), n)
		}
		// A task with two selector expressions of values values each.
		expressions = func(values int) []byte {
			nums := path(&taskv1.GetTaskResponse{}, "task", "spec", "runner_selector", "match_expressions")
			expression := field(nums[3], emptyEntries(path(&taskv1.SelectorRequirement{}, "values")[0], values))
			return within(nums[:3], bytes.Repeat(expression, 2))
		}
		unknown = protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 7)
	)
	// What kind of refusal a row expects.
	const (
		none     = ""
		items    = "items"    // ErrTooManyItems, and not ErrTooManyElements
		elements = "elements" // ErrTooManyElements
		broken   = "broken"   // another error
	)
	for _, tc := range []struct {
		name  string
		md    protoreflect.MessageDescriptor
		wire  []byte
		page  protowire.Number
		limit int
		want  string
	}{
		{"an empty message", getResponse, nil, 0, 0, none},
		{"a page of tasks up to the limit", listResponse, tasks(3, 0), tasksField, 3, none},
		{"a page of one task over the limit", listResponse, tasks(3, 0), tasksField, 2, items},
		{"entries of other fields do not count against the page limit", listResponse, tasks(2, 100), tasksField, 2, none},
		{"a message with no page takes any number of entries up to the budget", getResponse, taskOf(1000), 0, 0, none},
		{"conditions up to the budget", getResponse, taskOf(maxAnswerElements), 0, 0, none},
		{"one condition over the budget", getResponse, taskOf(maxAnswerElements + 1), 0, 0, elements},
		{"elements add up across the tree, up to the budget", listResponse, tasks(2, maxAnswerElements/2-1), tasksField, 2, none},
		{"elements add up across the tree, one over", listResponse, tasks(2, maxAnswerElements/2), tasksField, 2, elements},
		{"the page limit is met before the budget", listResponse, emptyEntries(tasksField, maxAnswerElements+1), tasksField, 1000, items},
		{"entries of a map count", getResponse, flood(&taskv1.GetTaskResponse{}, maxAnswerElements+1, "task", "metadata", "labels"), 0, 0, elements},
		{"entries of a repeated field in a repeated field count at both levels", getResponse, expressions(maxAnswerElements / 2), 0, 0, elements},
		{"values of a packed repeated scalar count each", listRequest, packedPhases(maxAnswerElements), 0, 0, none},
		{"one packed value over the budget", listRequest, packedPhases(maxAnswerElements + 1), 0, 0, elements},
		{"values of an unpacked repeated scalar count each", listRequest, unpackedPhases(maxAnswerElements + 1), 0, 0, elements},
		{"an unknown field is not counted", getResponse, bytes.Repeat(unknown, maxAnswerElements+1), 0, 0, none},
		{"a field of another wire type than the schema's is not counted", listResponse, bytes.Repeat(protowire.AppendVarint(protowire.AppendTag(nil, tasksField, protowire.VarintType), 1), maxAnswerElements+1), tasksField, 1, none},
		{"a group is skipped", getResponse, append(protowire.AppendTag(nil, 9, protowire.StartGroupType), protowire.AppendTag(nil, 9, protowire.EndGroupType)...), 0, 0, none},
		{"a length that overruns the message", listResponse, []byte{0x0a, 0x05, 'x'}, tasksField, 5, broken},
		{"a group that never ends", getResponse, protowire.AppendTag(nil, 9, protowire.StartGroupType), 0, 0, broken},
		{"a bad tag", getResponse, []byte{0x80}, 0, 0, broken},
		{"an answer that is over before it is bad", listResponse, append(emptyEntries(tasksField, 3), 0x80), tasksField, 2, items},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := scanElements(tc.wire, tc.md, tc.page, tc.limit)

			switch tc.want {
			case none:
				if err != nil {
					t.Fatalf("scanElements = %v, want none", err)
				}
			case items:
				if !errors.Is(err, ErrTooManyItems) || errors.Is(err, ErrTooManyElements) {
					t.Fatalf("scanElements = %v, want ErrTooManyItems and no ErrTooManyElements", err)
				}
			case elements:
				if !errors.Is(err, ErrTooManyElements) || !errors.Is(err, ErrTooManyItems) {
					t.Fatalf("scanElements = %v, want ErrTooManyElements", err)
				}
			case broken:
				if err == nil || errors.Is(err, ErrTooManyItems) {
					t.Fatalf("scanElements = %v, want an error about the bytes", err)
				}
			}
		})
	}
}

// A message nested deeper than protobuf decodes is refused by the scan, not walked.
func TestScanElementsRefusesDeepNesting(t *testing.T) {
	md := (&structpb.Value{}).ProtoReflect().Descriptor()
	listValue, values := path(&structpb.Value{}, "list_value")[0], path(&structpb.ListValue{}, "values")[0]
	// A list of a list of ... of an empty value, depth levels of lists.
	nested := func(depth int) []byte {
		b := []byte{}
		for range depth {
			b = field(listValue, field(values, b))
		}
		return b
	}
	if err := scanElements(nested(100), md, 0, 0); err != nil {
		t.Fatalf("scanElements of 100 levels = %v", err)
	}
	s := scanner{maxDepth: 50}
	if err := s.message(nested(100), md, 0); err == nil || errors.Is(err, ErrTooManyItems) {
		t.Fatalf("scan of 200 messages deep with a depth limit of 50 = %v, want a nesting error", err)
	}
}

// packedPhases is a ListTasksRequest that holds n phases packed in one entry.
func packedPhases(n int) []byte {
	phases := path(&taskv1.ListTasksRequest{}, "phases")[0]
	return field(phases, bytes.Repeat([]byte{0x02}, n))
}

// unpackedPhases is a ListTasksRequest that holds n phases, one entry each.
func unpackedPhases(n int) []byte {
	phases := path(&taskv1.ListTasksRequest{}, "phases")[0]
	return bytes.Repeat(protowire.AppendVarint(protowire.AppendTag(nil, phases, protowire.VarintType), 2), n)
}
