package nav

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/internal/uikit/policy"
)

// Logout only answers POST, which CrossOriginProtection keeps same-origin, so
// a link or an image on another site cannot end the session.
func TestLogoutIsAPostForm(t *testing.T) {
	var buf bytes.Buffer
	if err := VBar("agents", policy.Nav{ShowAgents: true}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	form := regexp.MustCompile(`(?s)<form method="POST" action="/logout"[^>]*>\s*<button[^>]*type="submit"`)
	if !form.MatchString(html) {
		t.Fatalf("no POST form with a submit button for /logout:\n%s", html)
	}
	if strings.Contains(html, `href="/logout"`) {
		t.Fatalf("logout is still a link:\n%s", html)
	}
}
