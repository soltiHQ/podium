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
)

// The REST listing of an agent's tasks passes the limit on to the agent, and takes
// no more tasks from a page than it asked for: an agent answers with what it
// likes, whatever it was asked.
func TestAgentTasksListRefusesOversizedPage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		items  int
		status int
	}{
		{"a page of as many tasks as the limit", 2, http.StatusOK},
		{"a page of one task more", 3, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := make([]string, tc.items)
			for i := range items {
				items[i] = fmt.Sprintf(`{"metadata":{"name":"task-%d","uid":"u%d"}}`, i, i)
			}
			body := `{"metadata":{},"items":[` + strings.Join(items, ",") + `]}`
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, body)
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
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status != http.StatusOK && strings.Contains(w.Body.String(), "task-") {
				t.Fatalf("the refused page was rendered: %s", w.Body.String())
			}
		})
	}
}
