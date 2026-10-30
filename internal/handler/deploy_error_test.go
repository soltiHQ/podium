package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/spec"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

func TestBlockedDeployReportsSafePlacementReasonWithoutSuccess(t *testing.T) {
	for _, tc := range []struct {
		name           string
		advertiseAgent bool
		message        string
	}{
		{"incompatible runner", true, "Deployment blocked: a target has no compatible runner. Review the placement preview."},
		{"missing target", false, "Deployment blocked: a target agent is missing from discovery. Review the placement preview."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := inmemory.New()
			saved, err := model.NewSpec("saved", "Saved spec", "lane")
			if err != nil {
				t.Fatal(err)
			}
			saved.SetTargets([]string{"private-agent"})
			saved.SetKindType(enum.TaskKindSubprocess)
			if err := store.UpsertSpec(ctx, saved); err != nil {
				t.Fatal(err)
			}
			if tc.advertiseAgent {
				agent, err := model.NewAgentFrom(model.AgentParams{ID: "private-agent", Name: "Private agent", Endpoint: "http://private-endpoint", EndpointType: 2, APIVersion: 1})
				if err != nil {
					t.Fatal(err)
				}
				agent.SetCapabilities([]model.RunnerCapability{{Name: "private-wasm-runner", Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Wasm"}}}})
				if err := store.UpsertAgent(ctx, agent); err != nil {
					t.Fatal(err)
				}
			}
			hub := event.NewHub(zerolog.Nop())
			api := &API{specSVC: spec.New(store, zerolog.Nop()), hub: hub, logger: zerolog.Nop()}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/specs/saved/deploy", nil)
			// Confirmation forms and the page's global error handler consume JSON.
			// A deploy-only user must not receive raw agent inventory details.
			r.Header.Set("HX-Request", "true")
			r.Header.Set("Accept", "application/json")
			r = r.WithContext(transportctx.WithIdentity(r.Context(), &identity.Identity{UserID: "operator", Permissions: []enum.Permission{enum.SpecsDeploy}}))
			w := httptest.NewRecorder()
			middleware.Negotiate(responder.NewJSON(), responder.NewHTML())(http.HandlerFunc(api.SpecsRouter)).ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("blocked deploy returned %d: %s", w.Code, w.Body.String())
			}
			var body struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != 400 || body.Message != tc.message {
				t.Fatalf("unsafe or generic blocked reason: %+v", body)
			}
			for _, private := range []string{"private-agent", "private-wasm-runner", "private-endpoint", "deploy rejected:"} {
				if strings.Contains(w.Body.String(), private) {
					t.Fatalf("raw details exposed: %s", w.Body.String())
				}
			}
			if w.Header().Get("HX-Trigger") != "" || w.Header().Get("HX-Redirect") != "" || len(hub.RecentEvents(10)) != 0 {
				t.Fatal("blocked deploy emitted success signal")
			}
			rollouts, err := storage.AllRollouts(ctx, store, nil)
			if err != nil || len(rollouts) != 0 {
				t.Fatalf("blocked deploy created rollouts: %v %v", rollouts, err)
			}
		})
	}
}

// deployFixture is a spec that targets the agents labelled env=prod, one such
// agent, and an operator who may preview and deploy it.
type deployFixture struct {
	store   *inmemory.Store
	hub     *event.Hub
	handler http.Handler
	actor   *identity.Identity
}

func newDeployFixture(t *testing.T) *deployFixture {
	t.Helper()
	store := inmemory.New()
	saved, err := model.NewSpec("saved", "Saved spec", "lane")
	if err != nil {
		t.Fatal(err)
	}
	saved.SetTargetLabels(map[string]string{"env": "prod"})
	if err := store.UpsertSpec(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	f := &deployFixture{
		store: store,
		hub:   event.NewHub(zerolog.Nop()),
		actor: &identity.Identity{UserID: "operator", Permissions: []enum.Permission{enum.SpecsGet, enum.AgentsGet, enum.SpecsDeploy}},
	}
	api := &API{specSVC: spec.New(store, zerolog.Nop()), hub: f.hub, logger: zerolog.Nop()}
	f.handler = middleware.Negotiate(responder.NewJSON(), responder.NewHTML())(http.HandlerFunc(api.SpecsRouter))
	f.agent(t, "agent-a")
	return f
}

// agent stores an agent that matches the spec's selector.
func (f *deployFixture) agent(t *testing.T, id string) {
	t.Helper()
	a, err := model.NewAgent(id, id, "http://"+id)
	if err != nil {
		t.Fatal(err)
	}
	a.LabelAdd("env", "prod")
	if err := f.store.UpsertAgent(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

// etag previews the placement as the API does for a JSON client and returns its ETag.
func (f *deployFixture) etag(t *testing.T) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/specs/saved/placement", nil)
	r = r.WithContext(transportctx.WithIdentity(r.Context(), f.actor))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("ETag") == "" {
		t.Fatalf("placement preview: %d, ETag %q", w.Code, w.Header().Get("ETag"))
	}
	return w.Header().Get("ETag")
}

// deploy posts Deploy as the confirmation dialog does, with an If-Match header
// for each given value; none sends no header.
func (f *deployFixture) deploy(t *testing.T, ifMatch ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/specs/saved/deploy", nil)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("Accept", "application/json")
	for _, v := range ifMatch {
		r.Header.Add("If-Match", v)
	}
	r = r.WithContext(transportctx.WithIdentity(r.Context(), f.actor))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func (f *deployFixture) rolloutAgents(t *testing.T) []string {
	t.Helper()
	rows, err := storage.AllRollouts(context.Background(), f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	agents := make([]string, 0, len(rows))
	for _, r := range rows {
		agents = append(agents, r.AgentID())
	}
	slices.Sort(agents)
	return agents
}

func TestDeployWithoutIfMatchKeepsWorking(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ifMatch []string
	}{{"no If-Match", nil}, {"If-Match *", []string{"*"}}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeployFixture(t)
			// Without a precondition Deploy takes the placement as it is now.
			f.agent(t, "agent-b")
			if w := f.deploy(t, tc.ifMatch...); w.Code != http.StatusNoContent {
				t.Fatalf("deploy: %d %s", w.Code, w.Body.String())
			}
			if got := f.rolloutAgents(t); !slices.Equal(got, []string{"agent-a", "agent-b"}) {
				t.Fatalf("rollouts for %v", got)
			}
			if len(f.hub.RecentEvents(10)) != 1 {
				t.Fatal("deploy recorded no event")
			}
		})
	}
}

func TestDeployIfMatchMismatchIs412(t *testing.T) {
	f := newDeployFixture(t)
	stale := f.etag(t)
	f.agent(t, "agent-b")
	current := f.etag(t)
	if current == stale {
		t.Fatal("a matching agent joined without changing the ETag")
	}

	// Every tag that does not name the current plan is refused. A weak tag never
	// matches (If-Match compares strongly); an empty tag and a blank header name
	// no plan.
	for _, ifMatch := range []string{stale, strings.Trim(stale, `"`), `""`, "", " ", "W/" + current} {
		w := f.deploy(t, ifMatch)
		if w.Code != http.StatusPreconditionFailed {
			t.Fatalf("If-Match %q: %d %s", ifMatch, w.Code, w.Body.String())
		}
		var body struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Code != http.StatusPreconditionFailed || body.Message != "Placement changed since it was previewed. Review it again." {
			t.Fatalf("If-Match %q: body %+v", ifMatch, body)
		}
		// The page refreshes the placement panel so the operator reviews the new plan.
		if w.Header().Get("HX-Trigger") != event.RefreshSpecs || w.Header().Get("HX-Redirect") != "" {
			t.Fatalf("If-Match %q: HX-Trigger %q HX-Redirect %q", ifMatch, w.Header().Get("HX-Trigger"), w.Header().Get("HX-Redirect"))
		}
		if got := f.rolloutAgents(t); len(got) != 0 || len(f.hub.RecentEvents(10)) != 0 {
			t.Fatalf("If-Match %q: refused deploy wrote rollouts %v or events", ifMatch, got)
		}
	}

	// The tag of the current plan deploys it, quoted as the ETag or bare. The
	// first deploy creates rollouts, which changes the plan: preview again.
	for _, quoted := range []bool{true, false} {
		ifMatch := f.etag(t)
		if !quoted {
			ifMatch = strings.Trim(ifMatch, `"`)
		}
		if w := f.deploy(t, ifMatch); w.Code != http.StatusNoContent {
			t.Fatalf("If-Match %q: %d %s", ifMatch, w.Code, w.Body.String())
		}
		if got := f.rolloutAgents(t); !slices.Equal(got, []string{"agent-a", "agent-b"}) {
			t.Fatalf("If-Match %q: rollouts for %v", ifMatch, got)
		}
	}
}

// If-Match is read as RFC 9110 lists, over every If-Match header: Deploy goes
// ahead when any listed strong tag names the current plan, quoted or bare, or
// with "*" anywhere. A weak tag never matches, and empty list elements name
// no plan.
func TestDeployIfMatchLists(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values func(stale, current string) []string
		want   int
	}{
		{"stale then current", func(s, c string) []string { return []string{s + ", " + c} }, http.StatusNoContent},
		{"current then stale", func(s, c string) []string { return []string{c + "," + s} }, http.StatusNoContent},
		{"current in a second header", func(s, c string) []string { return []string{s, c} }, http.StatusNoContent},
		{"bare current in a list", func(s, c string) []string { return []string{s + ", " + strings.Trim(c, `"`)} }, http.StatusNoContent},
		{"empty elements around current", func(_, c string) []string { return []string{" , " + c + " ,"} }, http.StatusNoContent},
		{"star after a stale tag", func(s, _ string) []string { return []string{s + ", *"} }, http.StatusNoContent},
		{"star in a second header", func(s, _ string) []string { return []string{s, "*"} }, http.StatusNoContent},
		{"stale tags only", func(s, _ string) []string { return []string{s + ", " + s, s} }, http.StatusPreconditionFailed},
		{"current only weak", func(s, c string) []string { return []string{s + ", W/" + c} }, http.StatusPreconditionFailed},
		{"current only weak, in a second header", func(s, c string) []string { return []string{s, "W/" + c} }, http.StatusPreconditionFailed},
		{"empty elements only", func(_, _ string) []string { return []string{" , ,", ","} }, http.StatusPreconditionFailed},
		{"stale and a blank header", func(s, _ string) []string { return []string{s, " "} }, http.StatusPreconditionFailed},
		{"quoted star", func(_, _ string) []string { return []string{`"*"`} }, http.StatusPreconditionFailed},
		{"current cut short", func(_, c string) []string { return []string{strings.TrimSuffix(c, `"`)} }, http.StatusPreconditionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeployFixture(t)
			stale := f.etag(t)
			f.agent(t, "agent-b")
			current := f.etag(t)

			w := f.deploy(t, tc.values(stale, current)...)
			if w.Code != tc.want {
				t.Fatalf("If-Match %q: %d %s, want %d", tc.values(stale, current), w.Code, w.Body.String(), tc.want)
			}
			wantAgents := []string{"agent-a", "agent-b"}
			if tc.want != http.StatusNoContent {
				wantAgents = []string{}
			}
			if got := f.rolloutAgents(t); !slices.Equal(got, wantAgents) {
				t.Fatalf("If-Match %q: rollouts for %v, want %v", tc.values(stale, current), got, wantAgents)
			}
		})
	}
}

// The parser behind TestDeployIfMatchLists: nil leaves Deploy unchecked, any
// other list, an empty one included, names the plans Deploy accepts.
func TestPlanTokensFromIfMatch(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   []string
	}{
		{nil, nil},
		{[]string{"*"}, nil},
		{[]string{`"a"`, `"b", *`}, nil},
		{[]string{`"a"`}, []string{"a"}},
		{[]string{"a"}, []string{"a"}},
		{[]string{`"a", "b"`, "c"}, []string{"a", "b", "c"}},
		{[]string{`W/"a", "b"`, `W/c`}, []string{"b"}},
		{[]string{""}, []string{}},
		{[]string{" , ,"}, []string{}},
		{[]string{`""`}, []string{""}},
		{[]string{`"a`}, []string{}},
		{[]string{`"*"`}, []string{"*"}},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/specs/saved/deploy", nil)
		for _, v := range tc.values {
			r.Header.Add("If-Match", v)
		}
		got := planTokensFromIfMatch(r)
		if (got == nil) != (tc.want == nil) || !slices.Equal(got, tc.want) {
			t.Errorf("If-Match %q: %#v, want %#v", tc.values, got, tc.want)
		}
	}
}
