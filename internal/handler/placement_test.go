package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/service/spec"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

func TestPlacementRequiresBothReadPermissionsAndNeverCreatesRollouts(t *testing.T) {
	st := inmemory.New()
	ts, err := model.NewSpec("saved", "Saved spec", "lane")
	if err != nil {
		t.Fatal(err)
	}
	ts.SetTargets([]string{"missing"})
	if err := st.UpsertSpec(context.Background(), ts); err != nil {
		t.Fatal(err)
	}
	api := &API{specSVC: spec.New(st, zerolog.Nop())}
	for _, tc := range []struct {
		permissions []enum.Permission
		want        int
	}{{nil, 401}, {[]enum.Permission{enum.SpecsGet}, 403}, {[]enum.Permission{enum.AgentsGet}, 403}, {[]enum.Permission{enum.SpecsGet, enum.AgentsGet}, 200}} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/specs/saved/placement", nil)
		if tc.permissions != nil {
			r = r.WithContext(transportctx.WithIdentity(r.Context(), &identity.Identity{UserID: "reader", Permissions: tc.permissions}))
		}
		w := httptest.NewRecorder()
		api.SpecsRouter(w, r)
		if w.Code != tc.want {
			t.Fatalf("permissions %v: code %d body %s", tc.permissions, w.Code, w.Body.String())
		}
		if w.Code == 200 {
			var plan restv1.SpecPlacement
			if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			if plan.SpecID != "saved" || plan.SpecVersion != ts.Version() || plan.DeployAllowed || len(plan.Targets) != 1 || plan.Targets[0].EligibilityReason != "missing_agent" {
				t.Fatalf("unexpected plan %+v", plan)
			}
		}
	}
	rows, err := storage.AllRollouts(context.Background(), st, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("preview mutated rollouts: %v %v", rows, err)
	}
	current, err := st.GetSpec(context.Background(), "saved")
	if err != nil || current.Version() != ts.Version() || current.Generation() != ts.Generation() {
		t.Fatal("preview mutated spec")
	}
}

func TestPlacementETagIsThePlanToken(t *testing.T) {
	f := newDeployFixture(t)
	preview := func(htmx bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/specs/saved/placement", nil)
		if htmx {
			r.Header.Set("HX-Request", "true")
		}
		r = r.WithContext(transportctx.WithIdentity(r.Context(), f.actor))
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("placement: %d %s", w.Code, w.Body.String())
		}
		return w
	}

	w := preview(false)
	var plan restv1.SpecPlacement
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(plan.PlanToken) {
		t.Fatalf("plan_token %q is not 16 bytes of lowercase hex", plan.PlanToken)
	}
	if got := w.Header().Get("ETag"); got != `"`+plan.PlanToken+`"` {
		t.Fatalf("ETag %s, want the quoted plan_token %s", got, plan.PlanToken)
	}

	// The panel the UI swaps in carries the same token for the Deploy dialog.
	w = preview(true)
	if got := w.Header().Get("ETag"); got != `"`+plan.PlanToken+`"` {
		t.Fatalf("panel ETag %s, want %s", got, plan.PlanToken)
	}
	if !strings.Contains(w.Body.String(), `data-plan-token="`+plan.PlanToken+`"`) {
		t.Fatalf("panel misses the plan token: %s", w.Body.String())
	}
}
