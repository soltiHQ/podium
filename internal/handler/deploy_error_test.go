package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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

// deploy posts Deploy as the confirmation dialog does, with ifMatch as the
// If-Match header unless it is empty.
func (f *deployFixture) deploy(t *testing.T, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/specs/saved/deploy", nil)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("Accept", "application/json")
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
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
	for _, ifMatch := range []string{"", "*"} {
		t.Run("If-Match "+strconv.Quote(ifMatch), func(t *testing.T) {
			f := newDeployFixture(t)
			// Without a precondition Deploy takes the placement as it is now.
			f.agent(t, "agent-b")
			if w := f.deploy(t, ifMatch); w.Code != http.StatusNoContent {
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
	// matches (If-Match compares strongly) and an empty one names no plan.
	for _, ifMatch := range []string{stale, strings.Trim(stale, `"`), `""`, "W/" + current} {
		w := f.deploy(t, ifMatch)
		if w.Code != http.StatusPreconditionFailed {
			t.Fatalf("If-Match %s: %d %s", ifMatch, w.Code, w.Body.String())
		}
		var body struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Code != http.StatusPreconditionFailed || body.Message != "Placement changed since it was previewed. Review it again." {
			t.Fatalf("If-Match %s: body %+v", ifMatch, body)
		}
		// The page refreshes the placement panel so the operator reviews the new plan.
		if w.Header().Get("HX-Trigger") != event.RefreshSpecs || w.Header().Get("HX-Redirect") != "" {
			t.Fatalf("If-Match %s: HX-Trigger %q HX-Redirect %q", ifMatch, w.Header().Get("HX-Trigger"), w.Header().Get("HX-Redirect"))
		}
		if got := f.rolloutAgents(t); len(got) != 0 || len(f.hub.RecentEvents(10)) != 0 {
			t.Fatalf("If-Match %s: refused deploy wrote rollouts %v or events", ifMatch, got)
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
			t.Fatalf("If-Match %s: %d %s", ifMatch, w.Code, w.Body.String())
		}
		if got := f.rolloutAgents(t); !slices.Equal(got, []string{"agent-a", "agent-b"}) {
			t.Fatalf("If-Match %s: rollouts for %v", ifMatch, got)
		}
	}
}
