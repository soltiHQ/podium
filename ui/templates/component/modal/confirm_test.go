package modal

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// A confirmation renders what its caller puts in it under the message, before
// the form that sends the request, so that a message can list what the action
// does. Without children it renders as before.
func TestConfirmRendersItsChildrenUnderTheMessage(t *testing.T) {
	render := func(ctx context.Context, c templ.Component) string {
		t.Helper()
		var buf bytes.Buffer
		if err := c.Render(ctx, &buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	with := templ.WithChildren(context.Background(), templ.Raw("<ul><li>an item</li></ul>"))
	for name, c := range map[string]templ.Component{
		"Confirm":     Confirm("n", "A title", "The message", "Go", "/x", MethodDelete, VariantDanger),
		"ConfirmWith": ConfirmWith("n", "A title", "The message", "Go", "/x", MethodDelete, VariantDanger, nil),
	} {
		t.Run(name, func(t *testing.T) {
			out := render(with, c)
			message, item, form := strings.Index(out, "The message"), strings.Index(out, "<li>an item</li>"), strings.Index(out, "<form")
			if message < 0 || item < message || form < item {
				t.Fatalf("message at %d, item at %d, form at %d; want them in that order:\n%s", message, item, form, out)
			}
			if bare := render(context.Background(), c); strings.Contains(bare, "<ul>") || !strings.Contains(bare, "The message") {
				t.Fatalf("a confirmation without children renders %s", bare)
			}
		})
	}
}
