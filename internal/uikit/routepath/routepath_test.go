package routepath

import (
	"net/url"
	"strings"
	"testing"
)

func TestRoutepathEscapesIDs(t *testing.T) {
	if got, want := ApiAgentLabels("a/b c"), "/api/v1/agents/a%2Fb%20c/labels"; got != want {
		t.Fatalf("ApiAgentLabels = %q, want %q", got, want)
	}
}

// hostileID mixes every character that ends a path segment, a URL, an HTML
// attribute or a JavaScript string, plus non-ASCII text.
const hostileID = "a/b c?d#e%f'g\"h<i>j&k;l,m+n=o\u2028é🌍"

func TestEveryIDHelperEscapesItsSegment(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(string) string
		path string // {id} marks the dynamic segment
	}{
		{"PageUserInfoByID", PageUserInfoByID, "/users/info/{id}"},
		{"ApiUserByID", ApiUserByID, "/api/v1/users/{id}"},
		{"ApiUserEnable", ApiUserEnable, "/api/v1/users/{id}/enable"},
		{"ApiUserDisable", ApiUserDisable, "/api/v1/users/{id}/disable"},
		{"ApiUserSessions", ApiUserSessions, "/api/v1/users/{id}/sessions"},
		{"ApiUserPassword", ApiUserPassword, "/api/v1/users/{id}/password"},
		{"ApiUserRevokeSession", ApiUserRevokeSession, "/api/v1/session/{id}/revoke"},
		{"PageAgentInfoByID", PageAgentInfoByID, "/agents/info/{id}"},
		{"ApiAgentByID", ApiAgentByID, "/api/v1/agents/{id}"},
		{"ApiAgentLabels", ApiAgentLabels, "/api/v1/agents/{id}/labels"},
		{"ApiAgentTasks", ApiAgentTasks, "/api/v1/agents/{id}/tasks"},
		{"PageSpecInfoByID", PageSpecInfoByID, "/specs/info/{id}"},
		{"PageSpecEditByID", PageSpecEditByID, "/specs/edit/{id}"},
		{"ApiSpecByID", ApiSpecByID, "/api/v1/specs/{id}"},
		{"ApiSpecDeploy", ApiSpecDeploy, "/api/v1/specs/{id}/deploy"},
		{"ApiSpecPlacement", ApiSpecPlacement, "/api/v1/specs/{id}/placement"},
		{"ApiSpecSync", ApiSpecSync, "/api/v1/specs/{id}/sync"},
		{"ApiSpecForceDelete", ApiSpecForceDelete, "/api/v1/specs/{id}/force"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.fn(hostileID)
			if want := strings.Replace(tc.path, "{id}", url.PathEscape(hostileID), 1); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}

			// The server routes on the decoded path, so the ID must come back
			// as one segment of it, with nothing leaking into query or fragment.
			u, err := url.Parse(got)
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.Replace(tc.path, "{id}", hostileID, 1); u.Path != want || u.RawQuery != "" || u.Fragment != "" {
				t.Fatalf("parsed path %q query %q fragment %q, want path %q", u.Path, u.RawQuery, u.Fragment, want)
			}
		})
	}
}

func TestTaskAndHistoryHelpersEscapeEverySegment(t *testing.T) {
	if got, want := ApiAgentTask("a/b", "c d"), "/api/v1/agents/a%2Fb/tasks/c%20d"; got != want {
		t.Fatalf("ApiAgentTask = %q, want %q", got, want)
	}
	if got, want := ApiAgentTaskLogs("a/b", "c d", "u&v"), "/api/v1/agents/a%2Fb/tasks/c%20d/logs/stream?task_uid=u%26v"; got != want {
		t.Fatalf("ApiAgentTaskLogs = %q, want %q", got, want)
	}
	if got, want := PageHistoryByAgent("a&b c"), "/history?agent_id=a%26b+c"; got != want {
		t.Fatalf("PageHistoryByAgent = %q, want %q", got, want)
	}
}
