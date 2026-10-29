package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"
	"testing"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
)

// hostile ends a JavaScript string, an HTML attribute, a line and a script
// element, and starts a call. hostileLiteral spells it as a double-quoted
// JavaScript literal.
const (
	hostile        = "/x'+alert(1)+\"\\\n\u2028</script>&"
	hostileLiteral = `"/x'+alert(1)+\"\\\n\u2028\u003c/script\u003e\u0026"`
)

// attrValue returns an attribute as the browser sees it: the value from the
// rendered markup with its HTML entities decoded.
func attrValue(t *testing.T, markup, name string) string {
	t.Helper()
	m := regexp.MustCompile(` ` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindStringSubmatch(markup)
	if m == nil {
		t.Fatalf("attribute %s missing from %s", name, markup)
	}
	return html.UnescapeString(m[1])
}

// literalAfter decodes the JSON string literal that immediately follows marker
// in a script expression and returns its value together with the expression
// minus that literal. Anything else after marker means the value was spliced
// into code instead of into a quoted literal.
func literalAfter(t *testing.T, expr, marker string) (value, rest string) {
	t.Helper()
	i := strings.Index(expr, marker)
	if i < 0 {
		t.Fatalf("%q missing from %s", marker, expr)
	}
	start := i + len(marker)
	dec := json.NewDecoder(strings.NewReader(expr[start:]))
	if err := dec.Decode(&value); err != nil {
		t.Fatalf("%q is not followed by a string literal (%v): %s", marker, err, expr)
	}
	return value, expr[:start] + expr[start+int(dec.InputOffset()):]
}

func TestAgentLabelEditorEscapesAgentID(t *testing.T) {
	for i, id := range []string{
		`x',{}).then(()=>0);alert(1);fetch('`,
		`x');alert(1);('`,
		`x"+alert(1)+"`,
		`x\');alert(1);//`,
		"x\n\u2028alert(1)",
		`</script><img src=x onerror=alert(1)>`,
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			var buf bytes.Buffer
			a := restv1.Agent{ID: id, Name: "agent", Status: "active", Labels: map[string]string{"env": "prod"}}
			if err := Detail(a, policy.AgentDetail{CanEditLabels: true, CanViewHistory: true}).Render(context.Background(), &buf); err != nil {
				t.Fatal(err)
			}
			expr := attrValue(t, buf.String(), "x-on:submit.prevent")

			// The URL is fetch's first argument and must be one string literal
			// that decodes back to the labels endpoint of exactly this agent.
			url, rest := literalAfter(t, expr, "fetch(")
			if want := routepath.ApiAgentLabels(id); url != want {
				t.Fatalf("fetch URL = %q, want %q", url, want)
			}

			// Nothing the agent chose may remain outside that literal.
			if strings.Contains(rest, "alert") || strings.Contains(expr, "');alert(1)") {
				t.Fatalf("agent ID leaked into code:\n%s", expr)
			}
		})
	}
}

func TestKVSubmitExprQuotesActionAndEventAsLiterals(t *testing.T) {
	expr := kvSubmitExpr(hostile, hostile)
	for _, want := range []string{
		"fetch(" + hostileLiteral + ", {",
		"htmx.trigger(document.body, " + hostileLiteral + ")",
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("missing %s in:\n%s", want, expr)
		}
	}
	if got := strings.Count(expr, "alert(1)"); got != 2 {
		t.Errorf("hostile text appears %d times, want 2 (once per literal):\n%s", got, expr)
	}
}

func TestKVEditorDataEncodesLabelsAsLiterals(t *testing.T) {
	got := kvEditorData(map[string]string{hostile: hostile})
	want := `{ rows: [{"key":` + hostileLiteral + `,"value":` + hostileLiteral + `}], submitting: false, error: '' }`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
