package workloadschema

import (
	"errors"
	"math/big"
	"regexp/syntax"
	"strings"
	"testing"
	"unicode"
)

// TestRegexSizeBoundsTheProgram holds the two measures of a regular
// expression to the program Go compiles from it. The weight that matching is
// charged by is never less than the program's instructions, so that matching
// runs no more than it charges, and not much more for an expression of small
// classes. The size that Compile bounds is the weight with each character
// class counted by its ranges, which Go builds and compiles one by one.
func TestRegexSizeBoundsTheProgram(t *testing.T) {
	for _, expr := range []string{
		"", "a", "abc", "a*", "a+", "a?", "(a)", "a|b|c", "[a-z]+", ".*", "(?s).*", "^x$", `\bfoo\b`,
		"(?i)hello", "(a*)*", "(a|)*", "(a*)+", "x{0}", "a{3}", "a{2,5}", "a{0,5}", "a{3,}", "a{1,}", "a{0,}",
		"(ab){2,4}", "((a|b){2}c){3}", "(?:a{2}){3}", "a{1000}", `[\p{L}\p{N}]+`, `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`,
		`(\d+\.){3}\d+`, "(a|ab)(c|bcd)(d*)", "a{2,}b{3,7}?", "(?U)a+b*", `\Aabc\z`, "[^a]", "(?m)^a$",
	} {
		re, err := syntax.Parse(expr, syntax.Perl)
		if err != nil {
			t.Fatalf("Parse(%q): %v", expr, err)
		}
		weight, size := regexWeight(re), regexSize(re)
		prog, err := syntax.Compile(re.Simplify())
		if err != nil {
			t.Fatalf("Compile(%q): %v", expr, err)
		}
		actual := len(prog.Inst)
		if weight < actual || weight > 2*actual+4 && !strings.Contains(expr, `\p`) {
			t.Errorf("regexWeight(%q) = %d, want at least the %d instructions Go compiles, and not much more", expr, weight, actual)
		}
		if size < weight {
			t.Errorf("regexSize(%q) = %d, under its weight %d", expr, size, weight)
		}
	}
	// A class counts its ranges in the size, and a binary search over them in
	// the weight.
	letters, err := syntax.Parse(`\pL`, syntax.Perl)
	if err != nil {
		t.Fatal(err)
	}
	ranges := len(letters.Rune) / 2
	if got := regexSize(letters); got != ranges+2 {
		t.Errorf(`regexSize(\pL) = %d, want its %d ranges and the program's 2 instructions`, got, ranges)
	}
	if got := regexWeight(letters); got < 3 || got > 6 {
		t.Errorf(`regexWeight(\pL) = %d, want a few steps for a search of %d ranges`, got, ranges)
	}
}

// TestScanPatternReadsAsGoDoes reads patterns as Go's parser does: it
// counts each Unicode class escape, with a braced name or a one-letter one,
// inside a class and outside, and no escape that an escaped backslash or
// \Q...\E makes literal; it refuses every inline flag group, but no text that
// only looks like one; and it refuses what it cannot read.
func TestScanPatternReadsAsGoDoes(t *testing.T) {
	x := string(rune(0x1E942))
	for _, tc := range []struct {
		src     string
		classes int
		refusal string // what the error says, "" for none
	}{
		{`^[a-z0-9]+$`, 0, ""},
		{`\pL`, 1, ""},
		{`\p{L}`, 1, ""},
		{`\PN`, 1, ""},
		{`\p{^Greek}`, 1, ""},
		{`[\pL\pN_-]`, 2, ""},
		{`[\p{L}\P{Greek}]+\pN`, 3, ""},
		{`[\pN-B-` + x + `-B-` + x + `]`, 1, ""}, // the round-5 repro's items: \pN, then - B - U+1E942 ...
		{`[[:alpha:]\pL]`, 1, ""},
		{`[]\pL]`, 1, ""},    // a first ] is a literal
		{`[^]\pL]`, 1, ""},   // and after ^ too
		{`\x{5B}\pL`, 1, ""}, // \x{5B} is "[", one escape
		{`\x5B\pL`, 1, ""},
		{`\0\pL\101\pN`, 2, ""}, // octal escapes
		{`\\pL`, 1, ""},         // an escaped backslash, then "pL": the raw count is an upper bound
		{`\Q\pL\E\pN`, 2, ""},   // \Q...\E is literal text, but counted in the raw text
		{`\Q\pL`, 1, ""},
		{`(?:a)(?P<n>b)(?<m>c)`, 0, ""},
		{`^\(?i\)?$`, 0, "inline flag"}, // the raw text has (?i: an accepted over-refusal
		{`[(?i)]`, 0, "inline flag"},
		{`\Q(?i)\E`, 0, "inline flag"},
		{`[A-[:](?i)[B-` + x + `]:]]`, 0, "inline flag"}, // Go: the class [A-[:], then (?i)
		{`[A-[:][\PC]:]]`, 1, ""},                        // Go: the class [A-[:], then \PC, then ]
		{`[A-[:alpha:](?i)[`, 0, "inline flag"},
		{x + `[P-[:^digit:]\x{1E942}`, 0, ""}, // Go reads [P-[:^digit:] as a class
		{`[:><-[:alpha:]`, 0, ""},
		{`[a-\pL]`, 0, "cannot read"}, // Go refuses \p as a range's end
		{`(?i)a`, 0, "inline flag"},
		{`a(?-m)b`, 0, "inline flag"},
		{`(?s:.)`, 0, "inline flag"},
		{`(?U)a+`, 0, "inline flag"},
		{`[a-z](?im)`, 0, "inline flag"},
		{`\p{L`, 0, "cannot read"},
		{`a\`, 0, "cannot read"},
		{`[a`, 0, "cannot read"},
		{`\p`, 0, "cannot read"},
	} {
		classes, err := scanPattern(tc.src)
		switch {
		case tc.refusal == "" && (err != nil || classes != tc.classes):
			t.Errorf("scanPattern(%q) = %d, %v, want %d Unicode classes", tc.src, classes, err, tc.classes)
		case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
			t.Errorf("scanPattern(%q) = %d, %v, want an error that says %q", tc.src, classes, err, tc.refusal)
		}
	}
}

// TestUnicodeClassWorkIsTheLargestClass holds what a Unicode class escape
// counts to the most that Go's parser builds for one: the ranges it appends
// for each table that a name it accepts resolves to, unicode.Categories (the
// one-letter ones among them) and unicode.Scripts, which aliases resolve to
// as well, Any and ASCII, and Assigned, which is the negation of Cn; each
// negated, as \P{X} or \p{^X}, too. Without inline flags, Go folds no case.
func TestUnicodeClassWorkIsTheLargestClass(t *testing.T) {
	appended := func(tab *unicode.RangeTable, negated bool) int {
		n := 0
		for _, r := range tab.R16 {
			n += rangesOf(rune(r.Lo), rune(r.Hi), rune(r.Stride))
		}
		for _, r := range tab.R32 {
			n += rangesOf(rune(r.Lo), rune(r.Hi), rune(r.Stride))
		}
		if negated {
			n++ // the gaps between the ranges, and the one after the last
		}
		return n
	}
	largest, name := 0, ""
	check := func(n string, tab *unicode.RangeTable) {
		for _, negated := range []bool{false, true} {
			if got := appended(tab, negated); got > largest {
				largest, name = got, n
				if negated {
					name = `\P{` + n + `}`
				}
			}
		}
		// Go's parser takes the name: the table is one a pattern can reach.
		if _, err := syntax.Parse(`\p{`+n+`}`, syntax.Perl); err != nil {
			t.Errorf(`Parse(\p{%s}): %v`, n, err)
		}
	}
	for n, tab := range unicode.Categories {
		check(n, tab)
	}
	for n, tab := range unicode.Scripts {
		check(n, tab)
	}
	check("Assigned", unicode.Cn) // \p{Assigned} is the negation of Cn, and \P{Assigned} Cn itself
	check("ASCII", &unicode.RangeTable{R16: []unicode.Range16{{Lo: 0, Hi: unicode.MaxASCII, Stride: 1}}})
	check("Any", &unicode.RangeTable{R32: []unicode.Range32{{Lo: 0, Hi: unicode.MaxRune, Stride: 1}}})
	if unicodeClassWork != largest {
		t.Errorf("unicodeClassWork = %d, want %d, the ranges Go's parser appends for %s", unicodeClassWork, largest, name)
	}
}

// rangesOf is the ranges that Go's parser appends for a range of a Unicode
// table: one, or one for each rune when the stride skips runes.
func rangesOf(lo, hi, stride rune) int {
	if stride == 1 {
		return 1
	}
	return int((hi-lo)/stride) + 1
}

// TestStepsAreInt64 holds the arithmetic of steps to int64, which a 32-bit int
// would wrap: one match of a string of 256 KiB against a program of 2 000
// instructions charges over 3e9 steps.
func TestStepsAreInt64(t *testing.T) {
	sameType((*pattern).steps, (func(*pattern, int) int64)(nil))
	sameType((*multiple).steps, (func(*multiple, num) int64)(nil))
	sameType(lookupSteps, (func(int, int) int64)(nil))
	sameType((*evaluator).charge, (func(*evaluator, int64) bool)(nil))
	if got := (&pattern{weight: maxRegexSize}).steps(maxBodyBytes); got < 3e9 {
		t.Errorf("one match of %d bytes against a weight of %d charges %d steps, want over 3e9", maxBodyBytes, maxRegexSize, got)
	}
}

// TestNumbersAreExact reads numbers into their exact form: equal values, as
// JSON compares them, are equal structs, and the order and the multiples of
// numbers are those of their decimal values.
func TestNumbersAreExact(t *testing.T) {
	read := func(s string) num {
		t.Helper()
		n, err := parseNum(s)
		if err != nil {
			t.Fatalf("parseNum(%q): %v", s, err)
		}
		return n
	}
	for _, same := range [][]string{
		{"1", "1.0", "10e-1", "0.1e1", "1E0", "1.000e+0"},
		{"0", "-0", "0.000", "0e400", "-0.0e-400"},
		{"-120", "-1.2e2", "-120.00"},
		{"0.007", "7e-3", "70E-4"},
	} {
		first := read(same[0])
		for _, s := range same[1:] {
			if n := read(s); n != first {
				t.Errorf("parseNum(%q) = %+v, want %+v, as for %q", s, n, first, same[0])
			}
		}
	}
	ascending := []string{"-1e400", "-2", "-1.5", "-1", "-0.0001", "0", "1e-400", "0.1", "0.11", "1", "1.0000000000000000000001", "2", "10", "1e400"}
	for i := range ascending {
		for j := range ascending {
			a, b := read(ascending[i]), read(ascending[j])
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := a.compare(b); got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", ascending[i], ascending[j], got, want)
			}
		}
	}
	for s, want := range map[string]string{
		"0": "0", "1.50": "1.5", "100": "100", "-0.05": "-0.05", "1e21": "1e+21", "1e20": "100000000000000000000",
		"1e-7": "1e-7", "123e-10": "1.23e-8", "0.000001": "0.000001",
	} {
		if got := read(s).String(); got != want {
			t.Errorf("parseNum(%q).String() = %q, want %q", s, got, want)
		}
	}
	// Multiples, against math/big's rationals: the small path and the big one.
	values := []string{"0", "7", "-21", "0.07", "0.015", "1.005", "19.99", "1e300", "7e-300", "123456789012345678901234567890", "3.5", "1e-400", "-0.25"}
	divisors := []string{"7", "0.01", "0.5", "2.5", "1e-300", "0.001", "1234567890123456789012", "3"}
	for _, v := range values {
		for _, d := range divisors {
			m := newMultiple(read(d))
			rv, _ := new(big.Rat).SetString(v)
			rd, _ := new(big.Rat).SetString(d)
			want := new(big.Rat).Quo(rv, rd).IsInt()
			if got := m.divides(read(v)); got != want {
				t.Errorf("%s multipleOf %s = %v, want %v", v, d, got, want)
			}
		}
	}
	for _, refused := range []string{"1" + strings.Repeat("0", 100), "1e401", "1e-401", "1e99999"} {
		if _, err := parseNum(refused); err == nil {
			t.Errorf("parseNum(%q) succeeded, want it refused", refused)
		}
	}
}

// TestEvaluationChargesAsItGoes holds the evaluator to its budget: a budget
// one step short of what a validation takes stops it with the too-expensive
// error, and that validation's own steps let it finish.
func TestEvaluationChargesAsItGoes(t *testing.T) {
	s := mustCompile(t, deploySchema)
	// A body afresh for each evaluation: what one evaluation finds about a
	// body's values, the next does not pay for again.
	body := func() *value {
		t.Helper()
		v, verr := decodeBody(map[string]any{"name": "web", "ports": []any{80.0, 443.0}, "env": map[string]any{"A": "b"}})
		if verr != nil {
			t.Fatal(verr)
		}
		return v
	}
	e := &evaluator{left: maxSteps}
	if !e.eval(s.root, body(), true) {
		t.Fatalf("the body is valid, and evaluating it found %v", e.problems)
	}
	used := maxSteps - e.left
	if used <= 0 {
		t.Fatalf("evaluating the body took %d steps", used)
	}
	if err := s.root.validate(body(), used); err != nil {
		t.Errorf("validate with the %d steps it takes = %v, want nil", used, err)
	}
	if err := s.root.validate(body(), used-1); !errors.Is(err, ErrValidationTooExpensive) {
		t.Errorf("validate with %d steps, one short = %v, want ErrValidationTooExpensive", used-1, err)
	}
}

// sameType compiles only for two values of one type.
func sameType[T any](T, T) {}
