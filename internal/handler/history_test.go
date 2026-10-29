package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/service/history"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

func historyFixture(t *testing.T) *API {
	t.Helper()
	store := inmemory.New()
	for i, uid := range []string{"OriginalUID", "ReplacementUID"} {
		record, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "removed-agent", AgentName: "Offline", SpecID: "removed-spec", TaskName: "same-name", TaskUID: uid, Generation: 1, Attempt: 1, Status: "failed", CapturedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond), SourceResourceVersion: "first-capture-rv"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertExecution(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	service, err := history.New(store, history.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return &API{historySVC: service}
}

func callHistory(api *API, path string, permissions ...enum.Permission) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if len(permissions) > 0 {
		r = r.WithContext(transportctx.WithIdentity(r.Context(), &identity.Identity{UserID: "reader", Permissions: permissions}))
	}
	w := httptest.NewRecorder()
	api.History(w, r)
	return w
}

func TestArchiveRequiresAgentReadAndWorksWithoutLiveResources(t *testing.T) {
	api := historyFixture(t)
	for _, tc := range []struct {
		permissions []enum.Permission
		want        int
	}{{nil, 401}, {[]enum.Permission{enum.AgentsEdit}, 403}, {[]enum.Permission{enum.AgentsGet}, 200}} {
		w := callHistory(api, "/api/v1/history?agent_id=removed-agent&spec_id=removed-spec&task_name=same-name&task_uid=OriginalUID", tc.permissions...)
		if w.Code != tc.want {
			t.Fatalf("%v: code %d body %s", tc.permissions, w.Code, w.Body.String())
		}
		if w.Code == 200 {
			var page restv1.ListHistoryResponse
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || page.Items[0].TaskUID != "OriginalUID" || page.CaptureComplete || page.Source != "agent-poll" {
				t.Fatalf("archive identities/provenance lost %+v", page)
			}
		}
	}
}

func TestArchivePaginationPreservesDistinctIncarnations(t *testing.T) {
	api := historyFixture(t)
	w := callHistory(api, "/api/v1/history?agent_id=removed-agent&limit=1", enum.AgentsGet)
	var first restv1.ListHistoryResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &first) != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("first page %d %s", w.Code, w.Body.String())
	}
	w = callHistory(api, "/api/v1/history?agent_id=removed-agent&limit=1&cursor="+url.QueryEscape(first.NextCursor), enum.AgentsGet)
	var second restv1.ListHistoryResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &second) != nil || len(second.Items) != 1 || second.Items[0].TaskUID == first.Items[0].TaskUID || second.NextCursor != "" {
		t.Fatalf("second page %d %s", w.Code, w.Body.String())
	}
}

func TestArchiveRejectsMalformedFiltersAndBoundsPageSize(t *testing.T) {
	for _, query := range []string{"agent_id=removed%GG", "status=failed;task_uid=OldUID", "limit=-1", "limit=0", "limit=nope", "status=running", "task_uid=a&task_uid=b", "agent_id=" + strings.Repeat("x", 513)} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/history?"+query, nil)
		if _, err := historyQuery(r); err == nil {
			t.Fatalf("accepted %s", query)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/history?limit=999999", nil)
	query, err := historyQuery(r)
	if err != nil || query.Limit != 100 {
		t.Fatalf("unbounded limit %+v %v", query, err)
	}
}
