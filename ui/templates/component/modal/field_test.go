package modal

import (
	"strings"
	"testing"
)

// hostile ends a JavaScript string, an HTML attribute, a line and a script
// element, and starts a call. hostileLiteral spells it as a double-quoted
// JavaScript literal.
const (
	hostile        = "/x'+alert(1)+\"\\\n\u2028</script>&"
	hostileLiteral = `"/x'+alert(1)+\"\\\n\u2028\u003c/script\u003e\u0026"`
)

func TestSubmitExpressionsQuoteActionAndMethodAsLiterals(t *testing.T) {
	password := passwordSubmitExpr(hostile)
	if !strings.Contains(password, "fetch("+hostileLiteral+", {") {
		t.Errorf("password action is not a string literal:\n%s", password)
	}
	if got := strings.Count(password, "alert(1)"); got != 1 {
		t.Errorf("hostile text appears %d times, want 1 (inside the literal):\n%s", got, password)
	}

	form := formSubmitExpr(hostile, hostile, []Field{{ID: "subject"}}, []AsyncSelect{{ID: "permissions"}})
	for _, want := range []string{
		"fetch(" + hostileLiteral + ", {",
		"method: " + hostileLiteral + ",",
	} {
		if !strings.Contains(form, want) {
			t.Errorf("missing %s in:\n%s", want, form)
		}
	}
	if got := strings.Count(form, "alert(1)"); got != 2 {
		t.Errorf("hostile text appears %d times, want 2 (once per literal):\n%s", got, form)
	}
}

func TestInitExprQuotesEndpointsAsLiterals(t *testing.T) {
	expr := initExpr([]AsyncSelect{
		{ID: "role_ids", Endpoint: hostile, ValueKey: "id", LabelKey: "name"},
		{ID: "permissions", Endpoint: hostile},
	})
	if got := strings.Count(expr, "fetch("+hostileLiteral+").then(r => r.json())"); got != 2 {
		t.Errorf("%d endpoints are string literals, want 2:\n%s", got, expr)
	}
	if got := strings.Count(expr, "alert(1)"); got != 2 {
		t.Errorf("hostile text appears %d times, want 2 (once per literal):\n%s", got, expr)
	}
}

func TestEditFormDataEncodesValuesAsLiterals(t *testing.T) {
	got := editFormData(
		[]Field{{ID: "subject", Value: hostile}, {ID: "locked", Value: hostile, Disabled: true}},
		[]AsyncSelect{{ID: "permissions", Selected: []string{hostile, "users:get"}}},
	)
	for _, want := range []string{
		"subject: " + hostileLiteral + ",",
		"permissions: [" + hostileLiteral + `,"users:get"],`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "alert(1)"); n != 2 {
		t.Errorf("hostile text appears %d times, want 2 (once per literal):\n%s", n, got)
	}
}
