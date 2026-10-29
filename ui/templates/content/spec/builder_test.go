package spec

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
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

func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestBuilderExpressionsQuoteEndpointAndMethodAsLiterals(t *testing.T) {
	if got, want := builderSubmitExpr(hostile, hostile), "submit("+hostileLiteral+","+hostileLiteral+")"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := builderXData(hostile, `{"name":"x"}`); !strings.HasSuffix(got, ","+hostileLiteral+")") {
		t.Errorf("agents endpoint is not a string literal: %s", got)
	}
}

func TestBuilderPageEmbedsSpecAsJSONAndQuotesItsEndpoint(t *testing.T) {
	spec := restv1.Spec{ID: hostile, Name: hostile, Slot: hostile, Targets: []string{hostile}, TargetLabels: map[string]string{hostile: hostile}}
	markup := render(t, BuilderPage(&spec))

	// x-data is soltiSpecBuilder(<seed>, <presets>, <endpoint>); the seed
	// must be a JSON document that decodes to exactly the spec we passed.
	var seed restv1.Spec
	data := strings.TrimPrefix(attrValue(t, markup, "x-data"), "soltiSpecBuilder(")
	if err := json.NewDecoder(strings.NewReader(data)).Decode(&seed); err != nil {
		t.Fatalf("seed is not a JSON document (%v): %s", err, data)
	}
	if seed.ID != hostile || seed.Name != hostile || seed.Slot != hostile || len(seed.Targets) != 1 || seed.Targets[0] != hostile || seed.TargetLabels[hostile] != hostile {
		t.Fatalf("seed lost data: %+v", seed)
	}

	// The save endpoint is a string literal that decodes to this spec's URL.
	submit := attrValue(t, markup, "x-on:submit.prevent")
	url, rest := literalAfter(t, submit, "submit(")
	if want := routepath.ApiSpecByID(hostile); url != want {
		t.Fatalf("submit URL = %q, want %q", url, want)
	}
	if strings.Contains(rest, "alert") {
		t.Fatalf("spec ID leaked into code: %s", submit)
	}
}

func TestPresetAndModeChipsQuoteTheirArgumentAsLiteral(t *testing.T) {
	const value, literal = `a'b"c\d`, `"a'b\"c\\d"`

	preset := render(t, presetButton(value, "Label"))
	if got, want := attrValue(t, preset, "x-on:click"), "applyPreset("+literal+")"; got != want {
		t.Errorf("preset click = %s, want %s", got, want)
	}
	if got, want := attrValue(t, preset, "x-bind:class"), "backoff_preset === "+literal+" ? "; !strings.HasPrefix(got, want) {
		t.Errorf("preset class = %s, want prefix %s", got, want)
	}

	mode := render(t, subprocessModeChip(value, "Label"))
	if got, want := attrValue(t, mode, "x-on:click"), "subprocess_mode = "+literal; got != want {
		t.Errorf("mode click = %s, want %s", got, want)
	}
	if got, want := attrValue(t, mode, "x-bind:class"), "subprocess_mode === "+literal+" ? "; !strings.HasPrefix(got, want) {
		t.Errorf("mode class = %s, want prefix %s", got, want)
	}
}
