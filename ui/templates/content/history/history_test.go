package history

import (
	"bytes"
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
)

func TestHistoryCursorLinksRetainExactFilters(t *testing.T) {
	query := restv1.HistoryQuery{AgentID: "a/b&c", SpecID: "saved", TaskName: "same name", TaskUID: "OldUID", Status: "failed", Limit: 1, Cursor: "previous"}
	parsed, err := url.Parse(QueryURL("/history", query, "opaque+/=next"))
	if err != nil {
		t.Fatal(err)
	}
	values := parsed.Query()
	for key, want := range map[string]string{"agent_id": query.AgentID, "spec_id": query.SpecID, "task_name": query.TaskName, "task_uid": query.TaskUID, "status": "failed", "limit": "1", "cursor": "opaque+/=next"} {
		if values.Get(key) != want {
			t.Fatalf("%s lost: %s", key, parsed)
		}
	}
	parsed, _ = url.Parse(QueryURL("/history", query, ""))
	if parsed.Query().Has("cursor") {
		t.Fatal("first page retained cursor")
	}
}

func TestArchivedResultRetainsProvenanceAndHasNoLiveActions(t *testing.T) {
	exit := int32(0)
	page := restv1.ListHistoryResponse{Items: []restv1.ExecutionRecord{{ID: "archive-id", AgentID: "deleted-agent", AgentName: "<script>", TaskName: "old-task", TaskUID: "OldUID", Generation: 9007199254740993, Attempt: 2, Status: "failed", ExitCode: &exit, Error: "<script>", CapturedAt: "2026-10-02T03:00:00Z", SourceResourceVersion: "opaque-rv", WorkloadAPIVersion: "solti.io/v1", WorkloadKind: "Subprocess"}}, NextCursor: "next"}
	var buf bytes.Buffer
	if err := Results(page, restv1.HistoryQuery{Limit: 50}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{"OldUID", "9007199254740993", "attempt 2", "opaque-rv", "First captured by Podium", "2026-10-02 03:00:00 UTC", "Exit code: 0", "Subprocess", "Next page"} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q", want)
		}
	}
	for _, forbidden := range []string{"<script>", "hx-post", "hx-delete", "hx-put", "Cancel task", "logs/stream"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("archive contains %s", forbidden)
		}
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("diagnostics not escaped")
	}
}

func TestHistoryFilterChangesDoNotResubmitContinuation(t *testing.T) {
	var buf bytes.Buffer
	if err := Filters(restv1.HistoryQuery{AgentID: "offline", Cursor: "stale-cursor", Limit: 50}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	if strings.Contains(html, "stale-cursor") || strings.Contains(html, `name="cursor"`) {
		t.Fatal("filter changes retain previous continuation")
	}
	if !strings.Contains(html, `method="get" action="/history"`) || !strings.Contains(html, `name="task_uid"`) {
		t.Fatal(html)
	}
}

func TestHistoryCardUsesCapturedSpecNameAndRetainsFullTaskIdentity(t *testing.T) {
	taskName := "podium-" + strings.Repeat("a", 64)
	for _, tc := range []struct{ specName, want string }{{"Nightly backup", "Nightly backup"}, {"", taskName}} {
		page := restv1.ListHistoryResponse{Items: []restv1.ExecutionRecord{{TaskName: taskName, SpecName: tc.specName, SpecID: "captured-spec-id", TaskUID: "OriginalUID", Status: "succeeded", SourceResourceVersion: "capture-rv", CapturedAt: "2026-10-02T03:00:00Z"}}}
		var buf bytes.Buffer
		if err := Results(page, restv1.HistoryQuery{Limit: 50}).Render(context.Background(), &buf); err != nil {
			t.Fatal(err)
		}
		html := buf.String()
		heading := regexp.MustCompile(`<h2[^>]*>([^<]*)</h2>`).FindStringSubmatch(html)
		if len(heading) != 2 || heading[1] != tc.want {
			t.Fatalf("card heading %+v want %q", heading, tc.want)
		}
		if tc.specName != "" && !strings.Contains(html, "Spec name at capture") {
			t.Fatal("captured name could be mistaken for a live lookup")
		}
		details := strings.SplitN(html, "<details", 2)
		if len(details) != 2 {
			t.Fatal("identity details missing")
		}
		for _, want := range []string{"Task resource name", taskName, "OriginalUID", "captured-spec-id", "capture-rv", "First captured by Podium"} {
			if !strings.Contains(details[1], want) {
				t.Fatalf("details lost %q", want)
			}
		}
	}
}
