package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/soltiHQ/control-plane/api/gen/solti/agent/v1"
	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/workloadschema"
)

const (
	e2eAPIVersion = "test.solti.io/v1"
	e2eKind       = "Echo"
	// e2eSchemaS1 accepts a body whose message is a string; e2eSchemaS2 asks for
	// an integer, so it rejects the same body.
	e2eSchemaS1 = `{"type":"object","required":["message"],"properties":{"message":{"type":"string"}},"additionalProperties":false}`
	e2eSchemaS2 = `{"type":"object","required":["message"],"properties":{"message":{"type":"integer"}},"additionalProperties":false}`
	e2eBody     = `{"message":"hello"}`
)

// echoTaskServer is the Task API of a fake agent: it keeps the manifests it is
// asked to apply and answers them as a running task.
type echoTaskServer struct {
	taskv1.UnimplementedTaskServiceServer
	mu      sync.Mutex
	applied []*taskv1.TaskManifest
}

func (s *echoTaskServer) ApplyTask(_ context.Context, r *taskv1.ApplyTaskRequest) (*taskv1.ApplyTaskResponse, error) {
	s.mu.Lock()
	s.applied = append(s.applied, r.GetManifest())
	s.mu.Unlock()
	return &taskv1.ApplyTaskResponse{Task: &taskv1.Task{
		Metadata: &taskv1.ObjectMeta{Name: r.GetManifest().GetMetadata().GetName(), Uid: "uid", ResourceVersion: "1", Generation: 1},
		Spec:     r.GetManifest().GetSpec(),
		Status:   &taskv1.TaskStatus{ObservedGeneration: 1, Phase: taskv1.TaskPhase_TASK_PHASE_RUNNING},
	}}, nil
}

func (s *echoTaskServer) manifests() []*taskv1.TaskManifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.applied)
}

// e2eAgent is a fake agent: a Task API on a loopback port and a discovery
// client that syncs it, as the SDK would, over gRPC.
type e2eAgent struct {
	t         *testing.T
	id        string
	endpoint  string
	task      *echoTaskServer
	discovery discoverv1.DiscoverServiceClient
	schemas   map[string][]byte // fingerprint -> schema it can send
}

func newE2EAgent(t *testing.T, id string, discovery discoverv1.DiscoverServiceClient) *e2eAgent {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	task := &echoTaskServer{}
	taskv1.RegisterTaskServiceServer(srv, task)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return &e2eAgent{t: t, id: id, endpoint: lis.Addr().String(), task: task, discovery: discovery, schemas: map[string][]byte{}}
}

// sync sends one sync that declares the Echo type with fingerprint (none when
// empty) and offers the schemas of offer, and returns the fingerprints Podium
// wants.
func (a *e2eAgent) sync(fingerprint string, offer ...string) []string {
	a.t.Helper()
	req := &discoverv1.SyncRequest{
		Id: a.id, Name: a.id, Endpoint: a.endpoint, EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1,
		Capabilities: &agentv1.AgentCapabilities{Runners: []*agentv1.RunnerCapability{{
			Name:      "echo",
			Workloads: []*agentv1.WorkloadType{{ApiVersion: e2eAPIVersion, Kind: e2eKind, SchemaFingerprint: fingerprint}},
		}}},
	}
	for _, fp := range offer {
		req.Schemas = append(req.Schemas, &discoverv1.WorkloadSchema{Fingerprint: fp, Schema: a.schemas[fp]})
	}
	resp, err := a.discovery.Sync(context.Background(), req)
	if err != nil {
		a.t.Fatalf("%s sync: %v", a.id, err)
	}
	return resp.GetWantSchemas()
}

// e2eREST calls the REST API of a running app as an administrator.
type e2eREST struct {
	t     *testing.T
	base  string
	token string
}

func (c e2eREST) do(method, path, body string, header map[string]string) (int, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp.StatusCode, out
}

func (c e2eREST) specID(slot string) string {
	c.t.Helper()
	code, out := c.do(http.MethodGet, "/api/v1/specs", "", nil)
	var page restv1.ListSpecsResponse
	if code != http.StatusOK || json.Unmarshal(out, &page) != nil {
		c.t.Fatalf("list specs: HTTP %d %s", code, out)
	}
	for _, s := range page.Items {
		if s.Slot == slot {
			return s.ID
		}
	}
	c.t.Fatalf("no spec with slot %q in %s", slot, out)
	return ""
}

func (c e2eREST) placement(id string) map[string]restv1.PlacementTarget {
	c.t.Helper()
	code, out := c.do(http.MethodGet, "/api/v1/specs/"+id+"/placement", "", nil)
	var p restv1.SpecPlacement
	if code != http.StatusOK || json.Unmarshal(out, &p) != nil {
		c.t.Fatalf("placement: HTTP %d %s", code, out)
	}
	byAgent := map[string]restv1.PlacementTarget{}
	for _, tg := range p.Targets {
		byAgent[tg.AgentID] = tg
	}
	return byAgent
}

func e2eSpec(slot, fingerprint, body string, targets ...string) string {
	quoted, _ := json.Marshal(targets)
	return `{"name":"` + slot + `","slot":"` + slot + `","targets":` + string(quoted) + `,"restart_type":"never","timeout_ms":5000,"backoff_factor":1.5,"backoff_first_ms":1000,"backoff_max_ms":5000,` +
		`"workload":{"api_version":"` + e2eAPIVersion + `","kind":"` + e2eKind + `","schema_fingerprint":"` + fingerprint + `","body":` + body + `}}`
}

// An agent declares a workload type through gRPC discovery, Podium fetches its
// schema, validates a spec against it over REST, places and deploys it, and
// the agent's Task API receives the body as it was written; a second agent
// whose schema rejects the body is shown as incompatible, and a third that
// declares no fingerprint as unpublished.
func TestWorkloadTypesEndToEnd(t *testing.T) {
	r := startApp(t, func(c *config.Config) {
		c.AgentAuth.Require = false
		// The rollout runs on the sync runner's tick: a short one keeps the test fast.
		c.Sync.TickInterval = 200 * time.Millisecond
	}, nil)
	cfg := r.cfg
	waitFor(t, 10*time.Second, "the listeners bind", func() bool { return r.app.ui.Addr() != "" && r.app.grpc.Addr() != "" })
	conn, err := grpc.NewClient("passthrough:///"+r.app.grpc.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	discovery := discoverv1.NewDiscoverServiceClient(conn)
	rest := e2eREST{t: t, base: "http://" + r.app.ui.Addr(), token: bearerFor(t, r.app, cfg, "e2e-admin", enum.All...)}

	f1, err := workloadschema.Fingerprint([]byte(e2eSchemaS1))
	if err != nil {
		t.Fatal(err)
	}
	f2, err := workloadschema.Fingerprint([]byte(e2eSchemaS2))
	if err != nil {
		t.Fatal(err)
	}

	// 1. Discovery: agent A declares the type; its first sync is answered with
	// the fingerprint Podium lacks, and its next one sends the schema.
	a := newE2EAgent(t, "agent-a", discovery)
	a.schemas[f1] = []byte(e2eSchemaS1)
	if want := a.sync(f1); !slices.Equal(want, []string{f1}) {
		t.Fatalf("first sync of A wants %v, want [%s]", want, f1)
	}
	if want := a.sync(f1, f1); len(want) != 0 {
		t.Fatalf("sync that sends the schema wants %v", want)
	}
	code, out := rest.do(http.MethodGet, "/api/v1/workload-types", "", nil)
	var types []restv1.WorkloadType
	if code != http.StatusOK || json.Unmarshal(out, &types) != nil {
		t.Fatalf("workload types: HTTP %d %s", code, out)
	}
	if len(types) != 1 || types[0].APIVersion != e2eAPIVersion || types[0].Kind != e2eKind || len(types[0].Fingerprints) != 1 {
		t.Fatalf("catalog = %s", out)
	}
	if fp := types[0].Fingerprints[0]; fp.Fingerprint != f1 || !fp.Received || !slices.Equal(fp.Agents, []string{"agent-a"}) {
		t.Fatalf("catalog fingerprint = %+v, want %s received from agent-a", fp, f1)
	}

	// 2. Create: a body the schema rejects is 400 with its path; a valid one is accepted.
	code, out = rest.do(http.MethodPost, "/api/v1/specs", e2eSpec("bad", f1, `{"message":7}`, "agent-a"), nil)
	if code != http.StatusBadRequest || !strings.Contains(string(out), "/message") {
		t.Fatalf("invalid body: HTTP %d %s, want 400 naming /message", code, out)
	}
	code, out = rest.do(http.MethodPost, "/api/v1/specs", e2eSpec("only-a", f1, e2eBody, "agent-a"), nil)
	if code != http.StatusNoContent {
		t.Fatalf("valid body: HTTP %d %s", code, out)
	}
	onlyA := rest.specID("only-a")
	if tg := rest.placement(onlyA)["agent-a"]; !tg.Eligible || runnerReason(tg) != "compatible" {
		t.Fatalf("placement of A = %+v, want compatible", tg)
	}

	// 3. Deploy: A's Task API gets the workload, with the body as extension.raw.
	if code, out = rest.do(http.MethodPost, "/api/v1/specs/"+onlyA+"/deploy", "", nil); code != http.StatusNoContent {
		t.Fatalf("deploy to A: HTTP %d %s", code, out)
	}
	// The rollout applies it asynchronously.
	waitFor(t, 15*time.Second, "A receives the workload", func() bool { return len(a.task.manifests()) > 0 })
	applied := a.task.manifests()
	if len(applied) != 1 {
		t.Fatalf("A applied %d manifests, want 1", len(applied))
	}
	w := applied[0].GetSpec().GetWorkload()
	if w.GetApiVersion() != e2eAPIVersion || w.GetKind() != e2eKind {
		t.Fatalf("workload = %s %s, want %s %s", w.GetApiVersion(), w.GetKind(), e2eAPIVersion, e2eKind)
	}
	raw := w.GetExtension().GetSpec().GetRaw()
	var got, want map[string]any
	if json.Unmarshal(raw, &got) != nil || json.Unmarshal([]byte(e2eBody), &want) != nil || !bytes.Equal(mustJSON(t, got), mustJSON(t, want)) {
		t.Fatalf("extension.raw = %q, want the body %s", raw, e2eBody)
	}

	// 4. Incompatible target: B declares the type with S2, which rejects the body.
	b := newE2EAgent(t, "agent-b", discovery)
	b.schemas[f2] = []byte(e2eSchemaS2)
	if want := b.sync(f2); !slices.Equal(want, []string{f2}) {
		t.Fatalf("first sync of B wants %v, want [%s]", want, f2)
	}
	if want := b.sync(f2, f2); len(want) != 0 {
		t.Fatalf("sync of B that sends the schema wants %v", want)
	}
	code, out = rest.do(http.MethodPost, "/api/v1/specs", e2eSpec("a-and-b", f1, e2eBody, "agent-a", "agent-b"), nil)
	if code != http.StatusNoContent {
		t.Fatalf("spec for A and B: HTTP %d %s", code, out)
	}
	both := rest.specID("a-and-b")
	placed := rest.placement(both)
	if tg := placed["agent-a"]; !tg.Eligible || runnerReason(tg) != "compatible" {
		t.Fatalf("placement of A = %+v, want compatible", tg)
	}
	if tg := placed["agent-b"]; tg.Eligible || runnerReason(tg) != "schema_incompatible" {
		t.Fatalf("placement of B = %+v, want schema_incompatible", tg)
	}
	if code, out = rest.do(http.MethodPost, "/api/v1/specs/"+both+"/deploy", "", nil); code != http.StatusConflict {
		t.Fatalf("deploy to A and B: HTTP %d %s, want 409", code, out)
	}
	if n := len(b.task.manifests()); n != 0 {
		t.Fatalf("B received %d manifests, want none", n)
	}
	if n := len(a.task.manifests()); n != 1 {
		t.Fatalf("A holds %d manifests after the refused deploy, want its one", n)
	}

	// 5. Unpublished: C declares the type without a fingerprint.
	c := newE2EAgent(t, "agent-c", discovery)
	if want := c.sync(""); len(want) != 0 {
		t.Fatalf("sync of C wants %v", want)
	}
	code, out = rest.do(http.MethodPost, "/api/v1/specs", e2eSpec("with-c", f1, e2eBody, "agent-c"), nil)
	if code != http.StatusNoContent {
		t.Fatalf("spec for C: HTTP %d %s", code, out)
	}
	if tg := rest.placement(rest.specID("with-c"))["agent-c"]; tg.Eligible || runnerReason(tg) != "schema_unpublished" {
		t.Fatalf("placement of C = %+v, want schema_unpublished", tg)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// runnerReason is the placement reason of the one runner of a target.
func runnerReason(tg restv1.PlacementTarget) string {
	if len(tg.Runners) != 1 {
		return "runners: " + strings.Join(tg.RunnerNames, ",")
	}
	return tg.Runners[0].Reason
}
