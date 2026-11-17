package workloadschema

import (
	"errors"
	"math/big"
	"regexp/syntax"
	"strings"
	"testing"
)

// TestRegexSizeBoundsTheProgram holds the size that Compile measures a
// regular expression by to the program Go compiles from it: never less, so
// that no expression past the bounds reaches regexp.Compile, and not much
// more, so that the bounds refuse no more than they say.
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
		measured := regexSize(re)
		prog, err := syntax.Compile(re.Simplify())
		if err != nil {
			t.Fatalf("Compile(%q): %v", expr, err)
		}
		if actual := len(prog.Inst); measured < actual || measured > 2*actual+4 {
			t.Errorf("regexSize(%q) = %d, want at least the %d instructions Go compiles, and not much more", expr, measured, actual)
		}
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
