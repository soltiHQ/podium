package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

// pageBudget is what one ListTasks call may allocate in all, the bytes of the fake
// agent in the same process included. Reading a page of 4 MiB takes 8 to 20 MiB of
// it, as measured; building the items such a page holds takes hundreds.
const pageBudget = 64 << 20

// allocated returns how many bytes the process allocated while fn ran, from a
// collected heap: TotalAlloc counts every allocation, whether it is live at the
// end or not. The tests that use it must not run in parallel.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// emptyItemsPage is a JSON page of size bytes, the most the proxy reads, that
// holds as many empty items as fit: three bytes for each.
func emptyItemsPage(size int) []byte {
	const head, item, tail = `{"metadata":{},"items":[`, `{},`, `{}]}`
	return []byte(head + strings.Repeat(item, (size-len(head)-len(tail))/len(item)) + tail)
}

// wantTooManyItems fails unless err is a refused page: a list tasks decode error
// that says the page held too many items. It is not one the proxy refused for its
// size, which asks the caller for a smaller page, and errkind sorts it as the
// error of an agent.
func wantTooManyItems(t *testing.T, err, op error) {
	t.Helper()
	if !errors.Is(err, op) || !errors.Is(err, ErrDecode) || !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("%v: want a %v decode error that is ErrTooManyItems", err, op)
	}
	if errors.Is(err, ErrTooManyElements) {
		t.Fatalf("%v: a page of too many items is no flood of elements", err)
	}
	if errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("%v: a page of too many items is no page too large, a smaller one is not what to ask for", err)
	}
	if kind := errkind.Classify(err); kind != errkind.Internal {
		t.Fatalf("kind = %v, want Internal", kind)
	}
}

// A page of empty items takes three bytes on the wire and hundreds on the heap
// once built, so the 4 MiB the proxy reads is no bound on what it costs. A page of
// more items than the limit is refused before the items are built.
func TestHTTPListTasksRefusesExtraItemsBeforeDecode(t *testing.T) {
	page := emptyItemsPage(maxAgentResponseBytes)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(page)
	}))
	defer agent.Close()
	p := &httpProxyV1{endpoint: agent.URL, client: agent.Client()}

	var err error
	grew := allocated(func() { _, err = p.ListTasks(context.Background(), TaskFilter{Limit: 1000}) })

	t.Logf("the call allocated %d MiB", grew>>20)
	if grew > pageBudget {
		t.Errorf("the call allocated %d MiB, want under %d MiB", grew>>20, pageBudget>>20)
	}
	wantTooManyItems(t, err, ErrListTasks)
}

// An empty task takes two bytes on the wire: tag 1, the tasks field, and a length
// of 0. The same page of them, as many as the 4 MiB a client reads holds, costs
// the heap more than 500 MiB to build.
func TestGRPCListTasksRefusesExtraItemsBeforeDecode(t *testing.T) {
	page := bytes.Repeat([]byte{0x0a, 0x00}, maxAgentMessageBytes/2)
	p := newRawAnswers(answers{list: page}).proxy(t)

	var err error
	grew := allocated(func() { _, err = p.ListTasks(context.Background(), TaskFilter{Limit: 1000}) })

	t.Logf("the call allocated %d MiB", grew>>20)
	if grew > pageBudget {
		t.Errorf("the call allocated %d MiB, want under %d MiB", grew>>20, pageBudget>>20)
	}
	wantTooManyItems(t, err, ErrListTasks)
}

// listing is an agent of one transport, with a page that names the given task IDs
// and the given next cursor.
type listing struct {
	name  string
	agent func(t *testing.T, ids []string, cursor string) AgentProxy
}

var listings = []listing{
	{"http", func(t *testing.T, ids []string, cursor string) AgentProxy {
		t.Helper()
		items := make([]string, len(ids))
		for i, id := range ids {
			items[i] = fmt.Sprintf(`{"metadata":{"name":%q,"uid":"u"}}`, id)
		}
		body := fmt.Sprintf(`{"metadata":{"continue":%q},"items":[%s]}`, cursor, strings.Join(items, ","))
		agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		t.Cleanup(agent.Close)
		return &httpProxyV1{endpoint: agent.URL, client: agent.Client()}
	}},
	{"grpc", func(t *testing.T, ids []string, cursor string) AgentProxy {
		t.Helper()
		page := &taskv1.ListTasksResponse{Continue: cursor}
		for _, id := range ids {
			page.Tasks = append(page.Tasks, &taskv1.Task{Metadata: &taskv1.ObjectMeta{Name: id, Uid: "u"}})
		}
		return grpcContractProxy(t, &taskContractServer{list: page})
	}},
}

// names are n task IDs.
func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("task-%04d", i)
	}
	return out
}

// A page may hold as many items as the limit says, and no more. A request that
// names none leaves the page size to the agent, whose pages hold 1000 at most.
func TestListTasksPageBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		items int
		ok    bool
	}{
		{"as many as the limit", 2, 2, true},
		{"fewer than the limit", 2, 1, true},
		{"none", 2, 0, true},
		{"one more than the limit", 2, 3, false},
		{"as many as the most any limit allows", 1000, 1000, true},
		{"one more than the most any limit allows", 1000, 1001, false},
		{"as many as the most, with no limit", 0, 1000, true},
		{"one more than the most, with no limit", 0, 1001, false},
	} {
		for _, l := range listings {
			t.Run(l.name+"/"+tc.name, func(t *testing.T) {
				p := l.agent(t, names(tc.items), "")

				resp, err := p.ListTasks(context.Background(), TaskFilter{Limit: tc.limit})

				if !tc.ok {
					wantTooManyItems(t, err, ErrListTasks)
					return
				}
				if err != nil || len(resp.Tasks) != tc.items || resp.Total != tc.items {
					t.Fatalf("ListTasks = %+v, %v, want a page of %d tasks", resp, err, tc.items)
				}
			})
		}
	}
}

// A task ID may be MaxTaskIDBytes long. A page that lists a longer one is refused
// as a page that could not be decoded: the runner keeps the IDs of a listing.
func TestListTasksRefusesLongTaskID(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		ok   bool
	}{
		{"an ID of the longest length", strings.Repeat("x", MaxTaskIDBytes), true},
		{"an ID one byte longer", strings.Repeat("x", MaxTaskIDBytes+1), false},
	} {
		for _, l := range listings {
			t.Run(l.name+"/"+tc.name, func(t *testing.T) {
				p := l.agent(t, []string{"task-0000", tc.id}, "")

				resp, err := p.ListTasks(context.Background(), TaskFilter{Limit: 10})

				if tc.ok {
					if err != nil || len(resp.Tasks) != 2 || resp.Tasks[1].ID != tc.id {
						t.Fatalf("ListTasks = %+v, %v, want both tasks", resp, err)
					}
					return
				}
				if !errors.Is(err, ErrListTasks) || !errors.Is(err, ErrDecode) {
					t.Fatalf("ListTasks = %v, want a list tasks decode error", err)
				}
				if errors.Is(err, ErrTooManyItems) || errors.Is(err, ErrResponseTooLarge) {
					t.Fatalf("ListTasks = %v: a long ID is neither too many items nor a page too large", err)
				}
				if kind := errkind.Classify(err); kind != errkind.Internal {
					t.Fatalf("kind = %v, want Internal", kind)
				}
				if len(err.Error()) > 1<<10 {
					t.Fatalf("the error is %d bytes long: does it hold the ID?", len(err.Error()))
				}
			})
		}
	}
}

// contentTypeServer reports the content-type of every ListTasks call.
type contentTypeServer struct {
	taskContractServer
	got chan<- []string
}

func (s *contentTypeServer) ListTasks(ctx context.Context, r *taskv1.ListTasksRequest) (*taskv1.ListTasksResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.got <- md.Get("content-type")
	return s.taskContractServer.ListTasks(ctx, r)
}

// The codec that counts the items of a page is the proto codec for the agent: the
// request is the one a client without it sends, content-type included, and a page
// within the limit decodes whole.
func TestGRPCListTasksKeepsTheWireOfTheCall(t *testing.T) {
	got := make(chan []string, 1)
	page := &taskv1.ListTasksResponse{
		Tasks:           []*taskv1.Task{{Metadata: &taskv1.ObjectMeta{Name: "task-0", Uid: "u0"}}, {Metadata: &taskv1.ObjectMeta{Name: "task-1", Uid: "u1"}}},
		ResourceVersion: "rv-1",
		Continue:        "next",
	}
	p := grpcContractProxyOver(t, &contentTypeServer{taskContractServer: taskContractServer{list: page}, got: got}, nil)

	resp, err := p.ListTasks(context.Background(), TaskFilter{Limit: 2, Cursor: "c", LabelSelector: "env=prod", Slot: "slot", Status: "running"})

	if err != nil || len(resp.Tasks) != 2 || resp.Tasks[1].ID != "task-1" || resp.Total != 2 || resp.NextCursor != "next" || resp.ResourceVersion != "rv-1" {
		t.Fatalf("ListTasks = %+v, %v, want the page whole", resp, err)
	}
	if contentType := <-got; len(contentType) != 1 || contentType[0] != "application/grpc" {
		t.Fatalf("the server got content-type %q, want application/grpc, as any other call sends", contentType)
	}
}

// httpPage serves body as the answer to every call, and reads the page it makes.
func httpPage(t *testing.T, body string, limit int) (*proxyv1.ListTasksResponse, error) {
	t.Helper()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
	defer agent.Close()
	p := &httpProxyV1{endpoint: agent.URL, client: agent.Client()}
	return p.ListTasks(context.Background(), TaskFilter{Limit: limit})
}

// The items of a page are decoded one at a time, but the page is read as any
// other JSON is: in any order, keys of any case, what it does not use skipped.
func TestHTTPListTasksDecodesThePage(t *testing.T) {
	const item = `{"metadata":{"name":"%s","uid":"u"},"junk":{"deep":[[1],[2]]}}`
	one, two := fmt.Sprintf(item, "a"), fmt.Sprintf(item, "b")
	for _, tc := range []struct {
		name  string
		body  string
		limit int
		ids   []string
		bad   string // what the error is, when the page is refused
	}{
		{"no items", `{"metadata":{}}`, 2, nil, ""},
		{"items null", `{"items":null}`, 2, nil, ""},
		{"items empty", `{"items":[]}`, 2, nil, ""},
		{"items after the metadata and what the page does not use", `{"metadata":{"continue":"c","resourceVersion":"7"},"junk":[1,{"a":[]}],"items":[` + one + `,` + two + `]}`, 2, []string{"a", "b"}, ""},
		{"items before the metadata", `{"items":[` + one + `],"metadata":{"continue":"c","resourceVersion":"7"}}`, 2, []string{"a"}, ""},
		{"a key of another case", `{"ITEMS":[` + one + `]}`, 2, []string{"a"}, ""},
		{"items not an array", `{"items":5}`, 2, nil, "decode"},
		{"items an object", `{"items":{}}`, 2, nil, "decode"},
		{"an item that is not an object", `{"items":[5]}`, 2, nil, "decode"},
		{"an item of the wrong type", `{"items":[{"metadata":5}]}`, 2, nil, "decode"},
		{"the items cut short", `{"items":[` + one, 2, nil, "decode"},
		{"too many items", `{"items":[` + one + `,` + two + `,` + one + `]}`, 2, nil, "many"},
		{"the items key twice shares one bound", `{"items":[` + one + `],"items":[` + two + `]}`, 1, nil, "many"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := httpPage(t, tc.body, tc.limit)

			switch tc.bad {
			case "":
				if err != nil {
					t.Fatalf("ListTasks: %v", err)
				}
				var got []string
				for _, task := range resp.Tasks {
					got = append(got, task.ID)
				}
				if !slices.Equal(got, tc.ids) || resp.Total != len(tc.ids) {
					t.Fatalf("tasks = %v (total %d), want %v", got, resp.Total, tc.ids)
				}
				if strings.Contains(tc.body, `"continue":"c"`) && (resp.NextCursor != "c" || resp.ResourceVersion != "7") {
					t.Fatalf("cursor %q, version %q, want c and 7", resp.NextCursor, resp.ResourceVersion)
				}
			case "many":
				wantTooManyItems(t, err, ErrListTasks)
			default:
				if !errors.Is(err, ErrListTasks) || !errors.Is(err, ErrDecode) || errors.Is(err, ErrTooManyItems) {
					t.Fatalf("ListTasks = %v, want a list tasks decode error that is no ErrTooManyItems", err)
				}
			}
		})
	}
}
