package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/proxy"
)

// listingJSON is the JSON a listing answers with: tasks named task-0, task-1 and so on,
// each with the given conditions, and the given next cursor.
func listingJSON(tasks, conditions int, cursor string) string {
	items := make([]string, tasks)
	for i := range items {
		cs := make([]string, conditions)
		for j := range cs {
			cs[j] = `{"type":"Ready","status":"True"}`
		}
		items[i] = fmt.Sprintf(`{"metadata":{"name":"task-%d","uid":"u%d"},"status":{"conditions":[%s]}}`, i, i, strings.Join(cs, ","))
	}
	return fmt.Sprintf(`{"metadata":{"continue":%q},"items":[%s]}`, cursor, strings.Join(items, ","))
}

// The REST listing of an agent's tasks passes the limit on to the agent, and takes
// no more from a page than the proxy lets through: no more tasks than it asked for,
// no task with more conditions than a task may hold, no cursor over the limit. An
// agent answers with what it likes, whatever it was asked.
func TestAgentTasksListRefusesOversizedPage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"a page of as many tasks as the limit", listingJSON(2, 0, ""), http.StatusOK},
		{"a page of one task more", listingJSON(3, 0, ""), http.StatusInternalServerError},
		{"a task with as many conditions as it may hold", listingJSON(1, proxy.MaxTaskConditions, ""), http.StatusOK},
		{"a task with one condition more", listingJSON(1, proxy.MaxTaskConditions+1, ""), http.StatusInternalServerError},
		{"a cursor of the longest length", listingJSON(1, 0, strings.Repeat("c", proxy.MaxCursorBytes)), http.StatusOK},
		{"a cursor one byte longer", listingJSON(1, 0, strings.Repeat("c", proxy.MaxCursorBytes+1)), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer agent.Close()
			f := newAPIFixture(t)
			ag, err := model.NewAgentFrom(model.AgentParams{ID: "agent-1", Name: "agent-1", Endpoint: agent.URL, EndpointType: 2, APIVersion: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err = f.store.UpsertAgent(context.Background(), ag); err != nil {
				t.Fatal(err)
			}
			actor := &identity.Identity{UserID: "u", Permissions: []enum.Permission{enum.AgentsGet}}

			w := f.do(http.MethodGet, "/api/v1/agents/agent-1/tasks?limit=2", "", actor)

			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %.300s", w.Code, tc.status, w.Body.String())
			}
			if tc.status != http.StatusOK && strings.Contains(w.Body.String(), "task-") {
				t.Fatalf("the refused page was rendered: %.300s", w.Body.String())
			}
		})
	}
}
