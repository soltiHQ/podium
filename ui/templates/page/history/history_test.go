package history

import (
	"bytes"
	"context"
	"strings"
	"testing"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
)

func TestHistoryPageExplainsPartialCaptureAndOfflineAccess(t *testing.T) {
	var buf bytes.Buffer
	if err := History(policy.Nav{ShowHistory: true}, restv1.HistoryQuery{AgentID: "removed", Limit: 50}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{"Partial capture", "offline or removed", "Logs are not archived", "does not prove physical process shutdown", "/api/v1/history?agent_id=removed"} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q", want)
		}
	}
}
