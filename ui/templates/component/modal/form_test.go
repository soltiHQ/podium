package modal

import (
	"bytes"
	"context"
	"html"
	"regexp"
	"strings"
	"testing"
)

// The Reload link is for a form that sends a tag and knows where to reload; a
// create form has neither.
func TestFormModalOffersReloadOnlyWithAHref(t *testing.T) {
	render := func(pre Precondition) string {
		t.Helper()
		var buf bytes.Buffer
		if err := FormModal("edit-user", "Edit user", "/api/v1/users/u", "PUT", []Field{{ID: "subject"}}, nil, pre).Render(context.Background(), &buf); err != nil {
			t.Fatal(err)
		}
		return html.UnescapeString(buf.String())
	}

	with := render(Precondition{IfMatch: `"tag"`, ReloadHref: "/users/info/u"})
	link := regexp.MustCompile(`<a [^>]*x-show="stale"[^>]*>[^<]*</a>`).FindString(with)
	if !strings.Contains(link, `href="/users/info/u"`) || !strings.Contains(link, ">Reload</a>") {
		t.Errorf("no Reload link to the page: %q", link)
	}
	if !strings.Contains(with, `'If-Match': "\"tag\""`) {
		t.Errorf("the form does not send its entity tag:\n%s", with)
	}

	without := render(Precondition{})
	if strings.Contains(without, `x-show="stale"`) || strings.Contains(without, "If-Match") {
		t.Errorf("a form without a precondition offers a reload or sends If-Match:\n%s", without)
	}
}
