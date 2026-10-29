package modal

import (
	"bytes"
	"context"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

func TestOpenEventIsCaseStableCollisionFreeAndQuoteSafe(t *testing.T) {
	safe := regexp.MustCompile(`^modal:open:[0-9a-f]*$`)
	seen := map[string]string{}
	names := []string{"cancel-csl8s33plW58pvVzrApg7g", "cancel-csl8s33plw58pvvzrapg7g", "A", "a", "quotes'\"<>&\\\n", "Привет 🌍", ""}
	for _, name := range names {
		event := openEventName(name)
		if !safe.MatchString(event) || strings.ToLower(event) != event {
			t.Fatalf("HTML-unstable event for %q: %q", name, event)
		}
		if previous, ok := seen[event]; ok && previous != name {
			t.Fatalf("event collision %q and %q", previous, name)
		}
		seen[event] = name
		raw, err := hex.DecodeString(strings.TrimPrefix(event, "modal:open:"))
		if err != nil || string(raw) != name {
			t.Fatalf("identifier lost: %q %v", raw, err)
		}
		listener := "x-on:" + event + ".window"
		if xOn(name)[listener] != "show = true" {
			t.Fatalf("listener differs from dispatch: %+v", xOn(name))
		}
		if got := OpenEvent(name); got != "$dispatch('"+event+"')" {
			t.Fatalf("unsafe or mismatched dispatch: %s", got)
		}
		var buf bytes.Buffer
		if err := Confirm(name, "Cancel task", "Retain history", "Cancel task", "/cancel", MethodPost, VariantDanger).Render(context.Background(), &buf); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), listener+`="show = true"`) {
			t.Fatalf("rendered listener missing for %q: %s", name, buf.String())
		}
	}
}
