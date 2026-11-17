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

// TestPatternWorkCountsWhatParsingBuilds estimates, from the source, what Go's
// parser builds beyond reading a pattern: the ranges of each Unicode class,
// and, where case folding may be on, the runes of each bracket range that it
// folds one at a time.
func TestPatternWorkCountsWhatParsingBuilds(t *testing.T) {
	for _, tc := range []struct {
		src  string
		work int
		ok   bool
	}{
		{`^[a-z0-9]+$`, 0, true},
		{`\pL`, unicodeClassWork, true},
		{`[\p{L}\p{N}_-]`, 2 * unicodeClassWork, true},
		{`\\pL`, unicodeClassWork, true}, // counted, though a backslash escapes the backslash: the estimate errs high
		{`\P{^Greek}`, unicodeClassWork, true},
		{`[\x{42}-\x{1E942}]`, 0, true},                            // no case folding: one range, appended at once
		{`(?i)[\x{42}-\x{1E942}]`, (0x1E942 - 0x42 + 1) / 4, true}, // folded rune by rune
		{`(?i)[a-f0-9]`, 6 / 4, true},                              // 0-9 lies below the folded runes
		{`(?i)[\x00-\x{10FFFF}]`, 0, true},                         // the whole range, appended at once
		{`(?i)[A-\x{1E943}]`, 0, true},
		{`(?i)[B-\x{1E943}]`, (0x1E943 - 0x42 + 1) / 4, true},
		{`(?-i)[\x{42}-\x{1E942}]`, (0x1E942 - 0x42 + 1) / 4, true}, // the estimate assumes folding wherever the flag appears
		{`[(?i)]` + `[\x{42}-\x{1E942}]`, (0x1E942 - 0x42 + 1) / 4, true},
		{`(?i)\Q[\x{42}-\x{1E942}]\E`, 0, true}, // literal text
		{`(?i)[\101-\132]`, 26 / 4, true},       // octal: A to Z
		{`(?i)[\q-z]`, 0, false},                // a range whose start it cannot read, under folding
		{`(?i)\pL`, unicodeClassWork, true},     // the escape's count covers its folding
	} {
		work, ok := patternWork(tc.src)
		if work != tc.work || ok != tc.ok {
			t.Errorf("patternWork(%q) = %d, %v, want %d, %v", tc.src, work, ok, tc.work, tc.ok)
		}
	}
	// What the parser builds for any Unicode table stays within what an
	// escape counts: the table's ranges, and under case folding also its
	// fold table's, and their merge.
	built := func(tables ...*unicode.RangeTable) int {
		n := 0
		for _, t := range tables {
			if t == nil {
				continue
			}
			for _, r := range t.R16 {
				n += rangesOf(rune(r.Lo), rune(r.Hi), rune(r.Stride))
			}
			for _, r := range t.R32 {
				n += rangesOf(rune(r.Lo), rune(r.Hi), rune(r.Stride))
			}
		}
		return n
	}
	check := func(name string, tab, fold *unicode.RangeTable) {
		work := built(tab)
		if fold != nil { // (?i): the table and its fold table, then their merge
			re, err := syntax.Parse(`(?i)\p{`+name+`}`, syntax.Perl)
			if err != nil {
				t.Fatalf(`Parse((?i)\p{%s}): %v`, name, err)
			}
			work = max(work, built(tab)+built(fold)+len(re.Rune)/2)
		}
		if work > unicodeClassWork {
			t.Errorf(`\p{%s} makes the parser build %d ranges, over the %d an escape counts`, name, work, unicodeClassWork)
		}
	}
	for name, tab := range unicode.Categories {
		check(name, tab, unicode.FoldCategory[name])
	}
	for name, tab := range unicode.Scripts {
		check(name, tab, unicode.FoldScript[name])
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
