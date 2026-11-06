package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// resetSecret is the enrollment secret of the tests that reset a credential.
const resetSecret = "0123456789abcdef0123456789abcdef"

// text returns what the logger wrote.
func (l *lockedLogs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// fakeAgent is an agent that podium calls: it answers an empty task listing and
// keeps the Authorization of every call it gets, and the number of connections
// it accepted (gRPC).
type fakeAgent struct {
	endpoint     string // what it advertises at discovery
	endpointType string // ENDPOINT_TYPE_HTTP or ENDPOINT_TYPE_GRPC

	mu          sync.Mutex
	authorized  []string
	connections int
}

func (a *fakeAgent) record(authorization string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.authorized = append(a.authorized, authorization)
}

// calls returns the Authorization of every call so far, in order.
func (a *fakeAgent) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.authorized)
}

// dials returns how many connections the agent accepted.
func (a *fakeAgent) dials() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connections
}

// syncBody is the heartbeat the agent id sends to the HTTP discovery.
func (a *fakeAgent) syncBody(id string) string {
	return `{"id":"` + id + `","endpoint":"` + a.endpoint + `","endpointType":"` + a.endpointType + `","apiVersion":1}`
}

func newHTTPAgent(t *testing.T) *fakeAgent {
	t.Helper()
	a := &fakeAgent{endpointType: "ENDPOINT_TYPE_HTTP"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.record(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, listingJSON(0, 0, ""))
	}))
	t.Cleanup(srv.Close)
	a.endpoint = srv.URL
	return a
}

// taskLister answers every ListTasks with an empty page.
type taskLister struct {
	taskv1.UnimplementedTaskServiceServer
	agent *fakeAgent
}

func (s taskLister) ListTasks(ctx context.Context, _ *taskv1.ListTasksRequest) (*taskv1.ListTasksResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.agent.record(strings.Join(md.Get("authorization"), ","))
	return &taskv1.ListTasksResponse{}, nil
}

// countingListener counts the connections it accepts in the agent.
type countingListener struct {
	net.Listener
	agent *fakeAgent
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.agent.mu.Lock()
		l.agent.connections++
		l.agent.mu.Unlock()
	}
	return conn, err
}

func newGRPCAgent(t *testing.T) *fakeAgent {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &fakeAgent{endpointType: "ENDPOINT_TYPE_GRPC", endpoint: lis.Addr().String()}
	srv := grpc.NewServer()
	taskv1.RegisterTaskServiceServer(srv, taskLister{agent: a})
	go func() { _ = srv.Serve(countingListener{Listener: lis, agent: a}) }()
	t.Cleanup(srv.Stop)
	return a
}

// issuedBy sends the reset of agent id and decodes the credential it answers.
func issuedBy(t *testing.T, f *apiFixture, id string, actor *identity.Identity) (restv1.ResetAgentCredentialResponse, map[string]any) {
	t.Helper()
	w := f.do(http.MethodPost, "/api/v1/agents/"+id+"/credential", "", actor)
	if w.Code != http.StatusOK {
		t.Fatalf("reset of %s: %d %s, want 200", id, w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("the answer carries Cache-Control %q, want no-store: it holds a token", got)
	}
	var issued restv1.ResetAgentCredentialResponse
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	return issued, raw
}

// recorded returns the events of the kind that the hub holds, newest first.
func recorded(f *apiFixture, kind string) []event.Record {
	var out []event.Record
	for _, rec := range f.hub.RecentEvents(event.RingSize) {
		if rec.Kind == kind {
			out = append(out, rec)
		}
	}
	return out
}

// holdsNoneOf fails the test when what contains one of secrets.
func holdsNoneOf(t *testing.T, what, text string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Errorf("%s holds %q", what, secret)
		}
	}
}

// A reset ends the old token in both directions at once: discovery refuses a
// sync with it, and podium's next call to the agent presents the new token. The
// token travels in the answer to the reset, and in no event and no log line. The
// reset closes the pooled gRPC connection too, so the call after it dials afresh.
func TestResetCredentialRevokesBothDirections(t *testing.T) {
	for _, transport := range []struct {
		name  string
		agent func(*testing.T) *fakeAgent
	}{{"http", newHTTPAgent}, {"grpc", newGRPCAgent}} {
		t.Run(transport.name, func(t *testing.T) {
			var logs lockedLogs
			logger := zerolog.New(&logs)
			f := newAPIFixtureLogging(t, nil, logger, agent.WithEnrollSecret(resetSecret))
			admin := f.user(t, "admin", enum.RoleAdminID)
			fake := transport.agent(t)
			discovery := NewHTTPDiscovery(logger, agent.New(f.store, logger, agent.WithEnrollSecret(resetSecret)), f.hub, discoveryLimiter(), NewSyncBuckets(), nil, true)
			syncWith := func(token string) int { return syncHTTP(discovery, fake.syncBody("agent-1"), token).Code }
			oldToken := agent.DeriveToken(resetSecret, "agent-1", 0)

			if got := syncWith(oldToken); got != http.StatusOK {
				t.Fatalf("sync with the token the agent enrolled with: %d, want 200", got)
			}
			if w := f.do(http.MethodGet, "/api/v1/agents/agent-1/tasks", "", admin); w.Code != http.StatusOK {
				t.Fatalf("task listing before the reset: %d %s", w.Code, w.Body.String())
			}
			if got := fake.calls(); !slices.Equal(got, []string{"Bearer " + oldToken}) {
				t.Fatalf("the agent saw %q before the reset, want its token", got)
			}
			dialed := fake.dials()

			issued, raw := issuedBy(t, f, "agent-1", admin)
			if issued.AgentID != "agent-1" || issued.Token != agent.DeriveToken(resetSecret, "agent-1", 1) || issued.Generation == nil || *issued.Generation != 1 {
				t.Fatalf("answer %+v, want agent-1, its token at generation 1 and generation 1", issued)
			}
			if len(raw) != 3 {
				t.Fatalf("answer %v, want exactly agent_id, token and generation", raw)
			}

			if got := syncWith(oldToken); got != http.StatusUnauthorized {
				t.Fatalf("sync with the old token after the reset: %d, want 401", got)
			}
			if got := syncWith(issued.Token); got != http.StatusOK {
				t.Fatalf("sync with the new token: %d, want 200", got)
			}
			if w := f.do(http.MethodGet, "/api/v1/agents/agent-1/tasks", "", admin); w.Code != http.StatusOK {
				t.Fatalf("task listing after the reset: %d %s", w.Code, w.Body.String())
			}
			if got := fake.calls(); !slices.Equal(got, []string{"Bearer " + oldToken, "Bearer " + issued.Token}) {
				t.Fatalf("the agent saw %q, want the old token, then the new one", got)
			}
			if transport.name == "grpc" && fake.dials() != dialed+1 {
				t.Fatalf("the agent accepted %d connections after the reset, want %d: the pooled one was not closed", fake.dials(), dialed+1)
			}

			events := recorded(f, event.AgentCredentialReset)
			if len(events) != 1 || events[0].Payload.ID != "agent-1" || events[0].Payload.By != "admin" {
				t.Fatalf("events %+v, want one agent_credential_reset of agent-1 by admin", events)
			}
			everything := fmt.Sprintf("%+v %+v", f.hub.RecentEvents(event.RingSize), f.hub.RecentIssues(event.RingSize))
			holdsNoneOf(t, "the events", everything, oldToken, issued.Token, resetSecret)
			holdsNoneOf(t, "the logs", logs.text(), oldToken, issued.Token, resetSecret)
		})
	}
}

// Without an enrollment secret a reset issues a new random token of 32 bytes, as
// 64 lowercase hex digits, and stores it, because trust on first use stores
// tokens. The answer has no generation. The old token is refused at the next
// sync, the new one is accepted and is the one podium presents, and a second
// reset issues another.
func TestResetWithoutSecretIssuesRandomToken(t *testing.T) {
	var logs lockedLogs
	logger := zerolog.New(&logs)
	f := newAPIFixtureLogging(t, nil, logger)
	admin := f.user(t, "admin", enum.RoleAdminID)
	fake := newHTTPAgent(t)
	discovery := NewHTTPDiscovery(logger, agent.New(f.store, logger), f.hub, discoveryLimiter(), NewSyncBuckets(), nil, true)
	syncWith := func(token string) int { return syncHTTP(discovery, fake.syncBody("agent-1"), token).Code }

	const chosen = "token-the-agent-chose"
	if got := syncWith(chosen); got != http.StatusOK {
		t.Fatalf("first sync: %d, want 200", got)
	}
	first, raw := issuedBy(t, f, "agent-1", admin)
	if _, has := raw["generation"]; has || first.Generation != nil {
		t.Fatalf("answer %v, want no generation for a stored credential", raw)
	}
	if first.AgentID != "agent-1" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(first.Token) || first.Token == chosen {
		t.Fatalf("answer %+v, want a new token of 64 lowercase hex digits", first)
	}
	cred, err := f.store.GetAgentCredential(context.Background(), "agent-1")
	if err != nil || cred.Derived() || cred.Token() != first.Token {
		t.Fatalf("credential after the reset: %v, want a stored one with the new token", err)
	}

	if got := syncWith(chosen); got != http.StatusUnauthorized {
		t.Fatalf("sync with the old token: %d, want 401", got)
	}
	if got := syncWith(first.Token); got != http.StatusOK {
		t.Fatalf("sync with the new token: %d, want 200", got)
	}
	if w := f.do(http.MethodGet, "/api/v1/agents/agent-1/tasks", "", admin); w.Code != http.StatusOK {
		t.Fatalf("task listing: %d %s", w.Code, w.Body.String())
	}
	if got := fake.calls(); !slices.Equal(got, []string{"Bearer " + first.Token}) {
		t.Fatalf("the agent saw %q, want the new token", got)
	}

	second, _ := issuedBy(t, f, "agent-1", admin)
	if second.Token == first.Token || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(second.Token) {
		t.Fatalf("second reset issued %q after %q, want another random token", second.Token, first.Token)
	}
	if got := syncWith(first.Token); got != http.StatusUnauthorized {
		t.Fatalf("sync with the token of the first reset: %d, want 401", got)
	}
	holdsNoneOf(t, "the logs", logs.text(), chosen, first.Token, second.Token)
	holdsNoneOf(t, "the events", fmt.Sprintf("%+v", f.hub.RecentEvents(event.RingSize)), chosen, first.Token, second.Token)
}

// With agent_auth.require false no token is checked or sent, so there is no
// credential to reset: the reset answers 409 and changes nothing, records
// nothing. So does a derived credential at the last generation. With auth on,
// an agent with no credential is 404, and creates none.
func TestResetRefusals(t *testing.T) {
	f := newAPIFixtureLogging(t, nil, zerolog.Nop(), agent.WithAuthDisabled())
	admin := f.user(t, "admin", enum.RoleAdminID)
	ctx := context.Background()
	cred, err := model.NewStoredAgentCredential("agent-1", "chosen-token")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.UpsertAgentCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}

	w := f.do(http.MethodPost, "/api/v1/agents/agent-1/credential", "", admin)
	if w.Code != http.StatusConflict || !strings.Contains(strings.ToLower(responseMessage(t, w)), "agent authentication is off") {
		t.Fatalf("reset with agent auth off: %d %s, want 409 that says agent authentication is off", w.Code, w.Body.String())
	}
	after, err := f.store.GetAgentCredential(ctx, "agent-1")
	if err != nil || after.Derived() || after.Token() != "chosen-token" || !after.UpdatedAt().Equal(cred.UpdatedAt()) {
		t.Fatalf("credential after the refused reset: %v, want it unchanged", err)
	}
	if got := recorded(f, event.AgentCredentialReset); len(got) != 0 {
		t.Fatalf("the refused reset recorded %+v", got)
	}

	// A derived credential at the last generation has no next one.
	h := newAPIFixtureLogging(t, nil, zerolog.Nop(), agent.WithEnrollSecret(resetSecret))
	last, err := model.NewDerivedAgentCredential("agent-1", math.MaxUint32, agent.SecretID(resetSecret))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.store.UpsertAgentCredential(ctx, last); err != nil {
		t.Fatal(err)
	}
	if w = h.do(http.MethodPost, "/api/v1/agents/agent-1/credential", "", h.user(t, "admin", enum.RoleAdminID)); w.Code != http.StatusConflict || !strings.Contains(responseMessage(t, w), "last generation") {
		t.Fatalf("reset at the last generation: %d %s, want 409 that names the last generation", w.Code, w.Body.String())
	}
	if kept, err := h.store.GetAgentCredential(ctx, "agent-1"); err != nil || kept.Generation() != math.MaxUint32 || !kept.UpdatedAt().Equal(last.UpdatedAt()) {
		t.Fatalf("credential after the refused reset: %v, want it unchanged", err)
	}

	// With agent authentication on, an agent with no credential is not found.
	g := newAPIFixture(t)
	if w = g.do(http.MethodPost, "/api/v1/agents/nobody/credential", "", g.user(t, "admin", enum.RoleAdminID)); w.Code != http.StatusNotFound {
		t.Fatalf("reset of an agent with no credential: %d %s, want 404", w.Code, w.Body.String())
	}
	if _, err = g.store.GetAgentCredential(ctx, "nobody"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the refused reset created a credential: %v", err)
	}
	if got := recorded(g, event.AgentCredentialReset); len(got) != 0 {
		t.Fatalf("the refused reset recorded %+v", got)
	}

	// Agent authentication does not matter to a delete.
	ag, err := model.NewAgent("agent-1", "agent-1", "http://10.0.0.5:8085")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.UpsertAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	if w = f.do(http.MethodDelete, "/api/v1/agents/agent-1", "", admin); w.Code != http.StatusNoContent {
		t.Fatalf("delete with agent auth off: %d %s, want 204", w.Code, w.Body.String())
	}
}

// seedDeletable stores agent-2, with a rollout of spec-a, next to the agent-1
// that discovery registered, with a rollout of spec-a and one of spec-b, a
// terminal run of each, and the label env=prod on agent-1.
func seedDeletable(t *testing.T, f *apiFixture) {
	t.Helper()
	ctx := context.Background()
	other, err := model.NewAgent("agent-2", "agent-2", "http://10.0.0.6:8085")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.UpsertAgent(ctx, other); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"spec-a", "agent-1"}, {"spec-b", "agent-1"}, {"spec-a", "agent-2"}} {
		ro, err := model.NewRollout(pair[0], pair[1], 1)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.store.UpsertRollout(ctx, ro); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"agent-1", "agent-2"} {
		rec, err := model.NewExecutionRecord(model.ExecutionRecordParams{
			AgentID: id, TaskName: "task", TaskUID: "uid-" + id, Generation: 1, Attempt: 1, Status: "succeeded", CapturedAt: time.Unix(1700000000, 0),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = f.store.UpsertExecution(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	ag, err := f.store.GetAgent(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	ag.LabelAdd("env", "prod")
	if err = f.store.UpsertAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
}

// rolloutsOf returns the IDs of the rollouts of the agent, sorted.
func rolloutsOf(t *testing.T, f *apiFixture, agentID string) []string {
	t.Helper()
	all, err := storage.AllRollouts(context.Background(), f.store, f.store.BuildRolloutFilter(storage.RolloutQueryCriteria{AgentID: agentID}))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(all))
	for _, ro := range all {
		ids = append(ids, ro.ID())
	}
	slices.Sort(ids)
	return ids
}

// Deleting an agent removes its row and every rollout of the agent, answers 204
// with a redirect to the agents page, records agent_removed with the actor, which
// is no issue, closes the agent's open agent_inactive and agent_disconnected
// issues as a reconnect does, and closes the pooled connection. The rollouts and
// the issues of other agents, the credential and the execution history stay. A
// delete that finds neither a row nor a rollout of the ID is 404, and nothing is
// recorded for it.
func TestDeleteAgentRemovesRowAndRollouts(t *testing.T) {
	ctx := context.Background()
	var logs lockedLogs
	logger := zerolog.New(&logs)
	f := newAPIFixtureLogging(t, nil, logger, agent.WithEnrollSecret(resetSecret))
	admin := f.user(t, "admin", enum.RoleAdminID)
	fake := newGRPCAgent(t)
	discovery := NewHTTPDiscovery(logger, agent.New(f.store, logger, agent.WithEnrollSecret(resetSecret)), f.hub, discoveryLimiter(), NewSyncBuckets(), nil, true)
	syncWith := func(token string) int { return syncHTTP(discovery, fake.syncBody("agent-1"), token).Code }
	token := agent.DeriveToken(resetSecret, "agent-1", 0)
	if got := syncWith(token); got != http.StatusOK {
		t.Fatalf("sync: %d, want 200", got)
	}
	seedDeletable(t, f)
	if w := f.do(http.MethodGet, "/api/v1/agents/agent-1/tasks", "", admin); w.Code != http.StatusOK {
		t.Fatalf("task listing: %d %s", w.Code, w.Body.String())
	}
	dialed := fake.dials()
	f.hub.Record(event.AgentInactive, event.Payload{ID: "agent-1", Name: "agent-1", By: "lifecycle"})
	f.hub.Record(event.AgentDisconnected, event.Payload{ID: "agent-1", Name: "agent-1", By: "lifecycle"})
	f.hub.Record(event.AgentInactive, event.Payload{ID: "agent-2", Name: "agent-2", By: "lifecycle"})

	w := f.do(http.MethodDelete, "/api/v1/agents/agent-1", "", admin)
	if w.Code != http.StatusNoContent || w.Header().Get("HX-Redirect") != "/agents" {
		t.Fatalf("delete: %d, HX-Redirect %q, %s; want 204 and a redirect to /agents", w.Code, w.Header().Get("HX-Redirect"), w.Body.String())
	}
	if _, err := f.store.GetAgent(ctx, "agent-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("agent-1 after the delete: %v, want not found", err)
	}
	if got := rolloutsOf(t, f, "agent-1"); len(got) != 0 {
		t.Fatalf("rollouts of agent-1 after the delete: %v, want none", got)
	}
	if got := rolloutsOf(t, f, "agent-2"); !slices.Equal(got, []string{model.RolloutID("spec-a", "agent-2")}) {
		t.Fatalf("rollouts of agent-2 after the delete of agent-1: %v, want its own", got)
	}
	if _, err := f.store.GetAgent(ctx, "agent-2"); err != nil {
		t.Fatalf("agent-2 after the delete of agent-1: %v", err)
	}
	if cred, err := f.store.GetAgentCredential(ctx, "agent-1"); err != nil || !cred.Derived() || cred.Generation() != 0 {
		t.Fatalf("credential of agent-1 after the delete: %v, want it kept as it was", err)
	}
	for _, id := range []string{"agent-1", "agent-2"} {
		records, err := storage.AllExecutions(ctx, f.store, f.store.BuildExecutionFilter(storage.ExecutionQueryCriteria{AgentID: id}))
		if err != nil || len(records) != 1 {
			t.Fatalf("execution history of %s after the delete: %d records %v, want 1", id, len(records), err)
		}
	}
	events := recorded(f, event.AgentRemoved)
	if len(events) != 1 || events[0].Payload.ID != "agent-1" || events[0].Payload.By != "admin" {
		t.Fatalf("events %+v, want one agent_removed of agent-1 by admin", events)
	}
	if got := recorded(f, event.AgentDeleted); len(got) != 0 {
		t.Fatalf("the delete recorded %+v: agent_deleted is lifecycle's", got)
	}
	if hasIssue(f, event.AgentRemoved, "agent-1") {
		t.Fatalf("issues %+v, want the delete not among them: agent_removed is no issue kind", f.hub.RecentIssues(event.RingSize))
	}
	if hasIssue(f, event.AgentInactive, "agent-1") || hasIssue(f, event.AgentDisconnected, "agent-1") || !hasIssue(f, event.AgentInactive, "agent-2") {
		t.Fatalf("issues %+v, want those of agent-1 closed and that of agent-2 kept", f.hub.RecentIssues(event.RingSize))
	}
	if closed := recorded(f, event.IssueClosed); len(closed) != 1 || closed[0].Payload.ID != "agent-1" || closed[0].Payload.By != "admin" {
		t.Fatalf("events %+v, want one issue_closed of agent-1 by admin", closed)
	}
	holdsNoneOf(t, "the logs", logs.text(), token, resetSecret)

	// The agent runs on: its next sync registers it again, and the call that
	// follows dials afresh, as the delete closed the pooled connection.
	if got := syncWith(token); got != http.StatusOK {
		t.Fatalf("sync after the delete: %d, want 200", got)
	}
	if w = f.do(http.MethodGet, "/api/v1/agents/agent-1/tasks", "", admin); w.Code != http.StatusOK {
		t.Fatalf("task listing after the delete: %d %s", w.Code, w.Body.String())
	}
	if fake.dials() != dialed+1 {
		t.Fatalf("the agent accepted %d connections after the delete, want %d: the pooled one was not closed", fake.dials(), dialed+1)
	}

	if w = f.do(http.MethodDelete, "/api/v1/agents/nobody", "", admin); w.Code != http.StatusNotFound {
		t.Fatalf("delete of an unknown agent: %d %s, want 404", w.Code, w.Body.String())
	}
	if w = f.do(http.MethodDelete, "/api/v1/agents/agent-1", "", admin); w.Code != http.StatusNoContent {
		t.Fatalf("delete of the agent that came back: %d %s, want 204", w.Code, w.Body.String())
	}
	if w = f.do(http.MethodDelete, "/api/v1/agents/agent-1", "", admin); w.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d %s, want 404", w.Code, w.Body.String())
	}
	if got := recorded(f, event.AgentRemoved); len(got) != 2 {
		t.Fatalf("events %+v, want one agent_removed for each of the two deletes that took place", got)
	}
}

// An agent that lifecycle deleted has no row, and keeps its rollouts. Its delete
// drops them in one transaction, answers 204 and records agent_removed with the
// actor, as the delete of an agent with a row does, and leaves the rollouts of
// other agents. A delete that finds neither a row nor a rollout of the ID answers
// 404, changes nothing and records nothing.
func TestDeleteDropsTheRolloutsOfAnAgentWithoutARow(t *testing.T) {
	ctx := context.Background()
	f := newAPIFixture(t)
	admin := f.user(t, "admin", enum.RoleAdminID)
	for _, pair := range [][2]string{{"spec-a", "agent-1"}, {"spec-b", "agent-1"}, {"spec-a", "agent-2"}} {
		ro, err := model.NewRollout(pair[0], pair[1], 1)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.store.UpsertRollout(ctx, ro); err != nil {
			t.Fatal(err)
		}
	}
	kept := []string{model.RolloutID("spec-a", "agent-2")}

	if w := f.do(http.MethodDelete, "/api/v1/agents/agent-1", "", admin); w.Code != http.StatusNoContent {
		t.Fatalf("delete of the rollouts of an agent without a row: %d %s, want 204", w.Code, w.Body.String())
	}
	if got := rolloutsOf(t, f, "agent-1"); len(got) != 0 {
		t.Fatalf("rollouts of agent-1 after the delete: %v, want none", got)
	}
	if got := rolloutsOf(t, f, "agent-2"); !slices.Equal(got, kept) {
		t.Fatalf("rollouts of agent-2 after the delete of agent-1: %v, want %v", got, kept)
	}
	events := recorded(f, event.AgentRemoved)
	if len(events) != 1 || events[0].Payload.ID != "agent-1" || events[0].Payload.By != "admin" {
		t.Fatalf("events %+v, want one agent_removed of agent-1 by admin", events)
	}

	for _, id := range []string{"agent-1", "nobody"} {
		if w := f.do(http.MethodDelete, "/api/v1/agents/"+id, "", admin); w.Code != http.StatusNotFound {
			t.Fatalf("delete of %s, which has neither a row nor a rollout: %d %s, want 404", id, w.Code, w.Body.String())
		}
	}
	if got := recorded(f, event.AgentRemoved); len(got) != 1 {
		t.Fatalf("events %+v, want no agent_removed for a delete that found nothing", got)
	}
	if got := rolloutsOf(t, f, "agent-2"); !slices.Equal(got, kept) {
		t.Fatalf("rollouts of agent-2 after the refused deletes: %v, want %v", got, kept)
	}
}

// An agent that still runs registers again on its next sync with its credential,
// derived or stored, which the delete kept as it was: it has no labels and no
// rollouts. Resetting the credential first keeps it out, since its old token is
// refused: only the new token brings it back.
func TestDeletedAgentReturnsWithoutRollouts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  []agent.Option
		token string
	}{
		{"derived credential", []agent.Option{agent.WithEnrollSecret(resetSecret)}, agent.DeriveToken(resetSecret, "agent-1", 0)},
		{"stored credential", nil, "token-the-agent-chose"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			logger := zerolog.Nop()
			f := newAPIFixtureLogging(t, nil, logger, tc.opts...)
			admin := f.user(t, "admin", enum.RoleAdminID)
			fake := newHTTPAgent(t)
			discovery := NewHTTPDiscovery(logger, agent.New(f.store, logger, tc.opts...), f.hub, discoveryLimiter(), NewSyncBuckets(), nil, true)
			syncWith := func(token string) int { return syncHTTP(discovery, fake.syncBody("agent-1"), token).Code }
			if got := syncWith(tc.token); got != http.StatusOK {
				t.Fatalf("first sync: %d, want 200", got)
			}
			seedDeletable(t, f)
			before, err := f.store.GetAgentCredential(ctx, "agent-1")
			if err != nil {
				t.Fatal(err)
			}

			if w := f.do(http.MethodDelete, "/api/v1/agents/agent-1", "", admin); w.Code != http.StatusNoContent {
				t.Fatalf("delete: %d %s", w.Code, w.Body.String())
			}
			if got := syncWith(tc.token); got != http.StatusOK {
				t.Fatalf("sync of the deleted agent with its credential: %d, want 200", got)
			}

			back, err := f.store.GetAgent(ctx, "agent-1")
			if err != nil {
				t.Fatalf("agent-1 did not come back: %v", err)
			}
			if labels := back.LabelsAll(); len(labels) != 0 {
				t.Fatalf("the agent that came back has the labels %v, want none", labels)
			}
			if got := rolloutsOf(t, f, "agent-1"); len(got) != 0 {
				t.Fatalf("the agent that came back has the rollouts %v, want none", got)
			}
			after, err := f.store.GetAgentCredential(ctx, "agent-1")
			if err != nil || after.Derived() != before.Derived() || after.Generation() != before.Generation() || after.Token() != before.Token() || !after.UpdatedAt().Equal(before.UpdatedAt()) {
				t.Fatalf("credential after the agent came back: %v, want it as it was", err)
			}

			// To keep the agent out: reset its credential first, then delete it.
			issued, _ := issuedBy(t, f, "agent-1", admin)
			if w := f.do(http.MethodDelete, "/api/v1/agents/agent-1", "", admin); w.Code != http.StatusNoContent {
				t.Fatalf("second delete: %d %s", w.Code, w.Body.String())
			}
			if got := syncWith(tc.token); got != http.StatusUnauthorized {
				t.Fatalf("sync with the old token after reset and delete: %d, want 401", got)
			}
			if _, err := f.store.GetAgent(ctx, "agent-1"); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("agent-1 came back with its old token: %v", err)
			}
			if got := syncWith(issued.Token); got != http.StatusOK {
				t.Fatalf("sync with the new token: %d, want 200", got)
			}
		})
	}
}

// Of the built-in roles only Admin may reset a credential or delete an agent,
// and each of the two permissions opens its own route: agents:reset the reset,
// agents:delete the delete, and agents:edit neither.
func TestOnlyAdminMayResetOrDeleteAnAgent(t *testing.T) {
	type routeCase struct {
		name, method, target string
		perm                 enum.Permission
		code                 int
	}
	routes := []routeCase{
		{"reset", http.MethodPost, "/api/v1/agents/agent-1/credential", enum.AgentsReset, http.StatusOK},
		{"delete", http.MethodDelete, "/api/v1/agents/agent-1", enum.AgentsDelete, http.StatusNoContent},
	}
	// seeded is an apiFixture that holds agent-1 and its credential.
	seeded := func(t *testing.T) *apiFixture {
		t.Helper()
		f := newAPIFixture(t)
		ag, err := model.NewAgent("agent-1", "agent-1", "http://10.0.0.5:8085")
		if err != nil {
			t.Fatal(err)
		}
		cred, err := model.NewStoredAgentCredential("agent-1", "chosen-token")
		if err != nil {
			t.Fatal(err)
		}
		if err = f.store.UpsertAgent(context.Background(), ag); err != nil {
			t.Fatal(err)
		}
		if err = f.store.UpsertAgentCredential(context.Background(), cred); err != nil {
			t.Fatal(err)
		}
		return f
	}
	for _, br := range enum.BuiltinRoles {
		holds := slices.Contains(br.Permissions, enum.AgentsReset) || slices.Contains(br.Permissions, enum.AgentsDelete)
		if holds != (br.ID == enum.RoleAdminID) {
			t.Errorf("the built-in role %s holds the agent reset or delete permission: %v", br.Name, holds)
		}
		for _, rt := range routes {
			t.Run(br.Name+" "+rt.name, func(t *testing.T) {
				f := seeded(t)
				want := http.StatusForbidden
				if br.ID == enum.RoleAdminID {
					want = rt.code
				}
				if w := f.do(rt.method, rt.target, "", f.user(t, "user-"+br.ID, br.ID)); w.Code != want {
					t.Fatalf("HTTP %d %s, want %d", w.Code, w.Body.String(), want)
				}
			})
		}
	}
	for _, p := range []enum.Permission{enum.AgentsEdit, enum.AgentsGet, enum.AgentsReset, enum.AgentsDelete} {
		for _, rt := range routes {
			t.Run(string(p)+" alone "+rt.name, func(t *testing.T) {
				f := seeded(t)
				want := http.StatusForbidden
				if rt.perm == p {
					want = rt.code
				}
				if w := f.do(rt.method, rt.target, "", &identity.Identity{UserID: "operator", Permissions: []enum.Permission{p}}); w.Code != want {
					t.Fatalf("HTTP %d %s, want %d", w.Code, w.Body.String(), want)
				}
			})
		}
	}
}

// Every event names its actor: the display name of the user, or for a user
// without one the subject, the name the user signs in with. The reset and the
// delete of an agent record it.
func TestEventsNameTheActorWithoutADisplayName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		actor identity.Identity
		want  string
	}{
		{"a display name", identity.Identity{UserID: "user-1", Subject: "operator", Name: "Operator"}, "Operator"},
		{"no display name", identity.Identity{UserID: "user-1", Subject: "operator"}, "operator"},
		{"neither a display name nor a subject", identity.Identity{UserID: "user-1"}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newAPIFixture(t)
			ag, err := model.NewAgent("agent-1", "agent-1", "http://10.0.0.5:8085")
			if err != nil {
				t.Fatal(err)
			}
			if err = f.store.UpsertAgent(ctx, ag); err != nil {
				t.Fatal(err)
			}
			cred, err := model.NewStoredAgentCredential("agent-1", "chosen-token")
			if err != nil {
				t.Fatal(err)
			}
			if err = f.store.UpsertAgentCredential(ctx, cred); err != nil {
				t.Fatal(err)
			}
			actor := tc.actor
			actor.Permissions = []enum.Permission{enum.AgentsReset, enum.AgentsDelete}

			if w := f.do(http.MethodPost, "/api/v1/agents/agent-1/credential", "", &actor); w.Code != http.StatusOK {
				t.Fatalf("reset: %d %s", w.Code, w.Body.String())
			}
			if w := f.do(http.MethodDelete, "/api/v1/agents/agent-1", "", &actor); w.Code != http.StatusNoContent {
				t.Fatalf("delete: %d %s", w.Code, w.Body.String())
			}
			for _, kind := range []string{event.AgentCredentialReset, event.AgentRemoved} {
				if events := recorded(f, kind); len(events) != 1 || events[0].Payload.By != tc.want {
					t.Fatalf("%s events %+v, want one by %q", kind, events, tc.want)
				}
			}
		})
	}
}
