package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
