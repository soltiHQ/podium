package jsexpr

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStringEscapesQuotesAndHTML(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", `""`},
		{"path", "/api/v1/agents/a%20b/labels", `"/api/v1/agents/a%20b/labels"`},
		{"single quote is harmless in a double-quoted literal", `it's`, `"it's"`},
		{"double quote", `say "hi"`, `"say \"hi\""`},
		{"backslash", `a\b`, `"a\\b"`},
		{"line breaks", "a\r\nb", `"a\r\nb"`},
		{"control characters", "a\x00b\x1fc", `"a\u0000b\u001fc"`},
		{"closing script tag", `</script><img src=x onerror=alert(1)>`, `"\u003c/script\u003e\u003cimg src=x onerror=alert(1)\u003e"`},
		{"ampersand", `a&amp;b`, `"a\u0026amp;b"`},
		{"line separator", "a\u2028b", `"a\u2028b"`},
		{"paragraph separator", "a\u2029b", `"a\u2029b"`},
		{"template literal syntax stays inert", "`${alert(1)}`", "\"`${alert(1)}`\""},
		{"breakout from a single-quoted fetch", `x',{}).then(()=>0);alert(1);fetch('`, `"x',{}).then(()=\u003e0);alert(1);fetch('"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := String(tc.in); got != tc.want {
				t.Fatalf("String(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestStringReplacesInvalidUTF8(t *testing.T) {
	lit := String("a\xffb")
	var out string
	if err := json.Unmarshal([]byte(lit), &out); err != nil || out != "a\ufffdb" || !utf8.ValidString(lit) {
		t.Fatalf("String of invalid UTF-8 = %q, decoded %q (err %v)", lit, out, err)
	}
}

// Whatever the input, the result must be one complete literal: the decoder
// stops exactly at its closing quote even when attacker-controlled code
// follows, and nothing that an HTML parser or a JavaScript lexer treats
// specially survives unescaped.
func TestStringNeverLeavesTheLiteral(t *testing.T) {
	for r := rune(0); r <= 0x3000; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		in := "a" + string(r) + `'"\` + string(r) + "z"
		lit := String(in)

		if strings.ContainsAny(lit, "<>&\u2028\u2029\n\r") {
			t.Fatalf("U+%04X: raw HTML or line terminator left in %q", r, lit)
		}
		for _, c := range lit {
			if c < 0x20 {
				t.Fatalf("U+%04X: raw control character left in %q", r, lit)
			}
		}

		var out string
		dec := json.NewDecoder(strings.NewReader(lit + ");alert(1)"))
		if err := dec.Decode(&out); err != nil || out != in || int(dec.InputOffset()) != len(lit) {
			t.Fatalf("U+%04X: %q is not one self-contained literal for %q (decoded %q, err %v)", r, lit, in, out, err)
		}
	}
}
