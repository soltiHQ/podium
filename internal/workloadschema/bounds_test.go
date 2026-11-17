package workloadschema

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// What one call of Validate may take, on the worst input within the bounds:
// the time without the race detector, and the bytes it allocates. The race
// detector slows the evaluator down several times over: under it, a call may
// take raceSlowdown times as long.
const (
	callLimit    = 250 * time.Millisecond
	callAlloc    = 64 << 20
	raceSlowdown = 16
)

// timeLimit is callLimit, or callLimit scaled for the race detector.
func timeLimit() time.Duration {
	if raceDetector {
		return callLimit * raceSlowdown
	}
	return callLimit
}

// measure runs f once and reports how long it took, how many bytes it
// allocated, and what it returned. No other test runs meanwhile: none is
// parallel.
func measure(f func() error) (time.Duration, uint64, error) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	err := f()
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	return took, after.TotalAlloc - before.TotalAlloc, err
}

// bounded validates body against s and fails the test unless Validate answers
// within timeLimit and callAlloc, with nil, a *ValidationError or the
// too-expensive error. It returns that answer.
func bounded(t *testing.T, name string, s *Schema, body map[string]any) error {
	t.Helper()
	took, alloc, err := measure(func() error { return s.Validate(body) })
	var verr *ValidationError
	if err != nil && !errors.As(err, &verr) && !errors.Is(err, ErrValidationTooExpensive) {
		t.Errorf("%s: Validate = %v (%T), want nil, a *ValidationError or ErrValidationTooExpensive", name, err, err)
	}
	if limit := timeLimit(); took > limit {
		t.Errorf("%s: Validate took %v, want at most %v", name, took.Round(time.Millisecond), limit)
	}
	if alloc > callAlloc {
		t.Errorf("%s: Validate allocated %d MiB, want at most %d", name, alloc>>20, callAlloc>>20)
	}
	t.Logf("%s: %v, %d KiB allocated: %v", name, took.Round(time.Microsecond), alloc>>10, briefly(err))
	return err
}

// briefly is an answer of Validate in a few words.
func briefly(err error) string {
	var verr *ValidationError
	switch {
	case err == nil:
		return "valid"
	case errors.As(err, &verr):
		return fmt.Sprintf("%d problems", len(verr.Problems))
	case errors.Is(err, ErrValidationTooExpensive):
		return "too expensive"
	}
	return err.Error()
}

// chainBody is a body nested depth objects deep, each the member "a" of the
// one around it: depth+1 values.
func chainBody(depth int) map[string]any {
	body := map[string]any{}
	for range depth {
		body = map[string]any{"a": body}
	}
	return body
}

// numbers is n zeros, as REST decodes them.
func numbers(n int) []any {
	xs := make([]any, n)
	for i := range xs {
		xs[i] = float64(0)
	}
	return xs
}

// repeated is n copies of item, separated by commas.
func repeated(item string, n int) string {
	return strings.TrimSuffix(strings.Repeat(item+",", n), ",")
}

// branching is an anyOf of k branches, each requiring the member "a" and
// applying the whole schema to it: on a chain of a members d deep, it fails
// at the bottom only, after k^d evaluations.
func branching(k int) string {
	return `{"anyOf":[` + repeated(`{"required":["a"],"properties":{"a":{"$ref":"#"}}}`, k) + `]}`
}

// fanOut is the schema of finding 5 of round 2: one patternProperties entry
// per pattern, each a $ref to an allOf of 30 $refs to an allOf of 31 leaves,
// which is 991 evaluations at the member a pattern matches.
func fanOut(patterns []string, leaf string) string {
	entries := make([]string, len(patterns))
	for i, p := range patterns {
		entries[i] = strconv.Quote(p) + `:{"$ref":"#/$defs/f"}`
	}
	return `{"$defs":{"w":{"allOf":[` + repeated(leaf, 31) + `]},"f":{"allOf":[` + repeated(`{"$ref":"#/$defs/w"}`, 30) + `]}},` +
		`"patternProperties":{` + strings.Join(entries, ",") + `}}`
}

// TestValidateBoundsTheProbesOfTheFindings validates the inputs that took the
// validator of rounds 1 and 2 past every bound: each answers within the time
// and the bytes a call may take, and with the answer it should give. Those
// that branch at each level of the body run out of their budget.
func TestValidateBoundsTheProbesOfTheFindings(t *testing.T) {
	const (
		valid     = "valid"
		problem   = "20 problems"
		tooCostly = "too expensive"
	)
	missing := make([]string, 47) // patterns that do not match "xs", as in the finding
	matching := make([]string, 47)
	for i := range missing {
		missing[i] = fmt.Sprintf("p%d", i)
		matching[i] = fmt.Sprintf("^xs|z%d", i)
	}
	// 20 arrays of 498 numbers, which differ in the last number only.
	nearlyEqual := make([]any, 20)
	for i := range nearlyEqual {
		xs := numbers(498)
		xs[497] = float64(i)
		nearlyEqual[i] = xs
	}
	unique := `{"$defs":{"u":{"allOf":[` + repeated(`{"uniqueItems":true}`, 31) + `]}},` +
		`"properties":{"x":{"allOf":[` + repeated(`{"$ref":"#/$defs/u"}`, 30) + `]}}}`
	for _, tc := range []struct {
		name   string
		schema string
		body   map[string]any
		want   string
	}{
		{"an anyOf chain 150 deep (I1, round 1)", anyOfChain(150), map[string]any{}, tooCostly},
		{"an anyOf chain 150 deep at a nested body", anyOfChain(150), chainBody(50), tooCostly},
		{"two branches into one member, 40 levels (F1, round 2)", branching(2), chainBody(40), tooCostly},
		{"150 branches into one member, 4 levels", branching(150), chainBody(4), tooCostly},
		{
			"150 branches into every member, 9 levels (I1c, round 2)",
			`{"anyOf":[` + repeated(`{"type":["object","number"],"additionalProperties":{"$ref":"#"}}`, 150) + `]}`,
			map[string]any{"m0": map[string]any{"m1": map[string]any{"m2": map[string]any{"m3": map[string]any{
				"m4": map[string]any{"m5": map[string]any{"m6": map[string]any{"m7": map[string]any{"m8": "leaf"}}}},
			}}}}},
			tooCostly,
		},
		{"a failure at each of 9 998 levels (F3, round 2)", `{"properties":{"a":{"$ref":"#"}},"required":["b"]}`, chainBody(9_998), problem},
		{"47 patterns at a 9 998-item array (finding 5)", fanOut(missing, `{}`), map[string]any{"xs": numbers(9_998)}, valid},
		{"47 matching patterns at a 9 998-item array", fanOut(matching, `{}`), map[string]any{"xs": numbers(9_998)}, valid},
		{"47 matching patterns of items at a 9 998-item array", fanOut(matching, `{"items":{"minimum":0}}`), map[string]any{"xs": numbers(9_998)}, tooCostly},
		{"uniqueItems at 20 arrays of 498 numbers, 930 times (finding 4)", unique, map[string]any{"x": nearlyEqual}, valid},
		{"format at a 21 000-byte string (finding 3)", `{"properties":{"x":{"format":"regex"}}}`, map[string]any{"x": strings.Repeat("a{1000}", 3000)}, valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := briefly(bounded(t, tc.name, mustCompile(t, tc.schema), tc.body)); got != tc.want {
				t.Errorf("%s: Validate answered %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// TestValidateRefusesWorkPastTheBudget stops a validation that would take
// more steps than the budget, with an error that says so, and leaves nothing
// running.
func TestValidateRefusesWorkPastTheBudget(t *testing.T) {
	before := runtime.NumGoroutine()
	for name, tc := range map[string]struct {
		schema string
		body   map[string]any
	}{
		"2^150 evaluations":               {anyOfChain(150), map[string]any{}},
		"2^40 evaluations":                {branching(2), chainBody(40)},
		"9 998 items under 7×930 schemas": {fanOut([]string{"^xs|z0", "^xs|z1", "^xs|z2", "^xs|z3", "^xs|z4", "^xs|z5", "^xs|z6"}, `{"items":true}`), map[string]any{"xs": numbers(9_998)}},
	} {
		err := bounded(t, name, mustCompile(t, tc.schema), tc.body)
		if !errors.Is(err, ErrValidationTooExpensive) || !strings.Contains(err.Error(), "steps") {
			t.Errorf("%s: Validate = %v, want ErrValidationTooExpensive for its steps", name, err)
		}
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("%d goroutines run after the validations, %d before", after, before)
	}
	// The same schemas on bodies they take few steps on validate as before:
	// a chain that ends in a number, which required does not read, is valid.
	if err := mustCompile(t, branching(2)).Validate(map[string]any{"a": map[string]any{"a": 1.0}}); err != nil {
		t.Errorf("Validate of a short chain: %v", err)
	}
	var verr *ValidationError
	if err := mustCompile(t, anyOfChain(3)).Validate(map[string]any{}); !errors.As(err, &verr) {
		t.Errorf("Validate against a short anyOf chain = %v, want its problem", err)
	}
}

// TestValidateRefusesEvaluationNestedTooDeep stops a validation that nests
// more than 1 000 evaluations, however few steps it takes.
func TestValidateRefusesEvaluationNestedTooDeep(t *testing.T) {
	s := mustCompile(t, `{"properties":{"a":{"$ref":"#"}}}`)
	// Each level of the body nests two evaluations: properties, then $ref.
	err := bounded(t, "a chain of 9 998 levels", s, chainBody(9_998))
	if !errors.Is(err, ErrValidationTooExpensive) || !strings.Contains(err.Error(), "deep") {
		t.Errorf("Validate of a chain of 9 998 levels = %v, want ErrValidationTooExpensive for its depth", err)
	}
	if err := s.Validate(chainBody(400)); err != nil {
		t.Errorf("Validate of a chain of 400 levels = %v, want nil", err)
	}
}

func TestValidateRefusesABodyPastItsBounds(t *testing.T) {
	// A body that validation would find a problem with: only the bound shows.
	s := mustCompile(t, `{"required":["missing"]}`)
	for name, tc := range map[string]struct {
		body map[string]any
		path string
		says string
	}{
		"over 256 KiB":                    {map[string]any{"s": strings.Repeat("x", maxBodyBytes)}, "", "bytes"},
		"over 10 000 values":              {map[string]any{"xs": numbers(maxBodyValues)}, "", "values"},
		"a number of 101 digits":          {map[string]any{"n": jsonNumber("1" + strings.Repeat("0", 100))}, "/n", "digits"},
		"a number of 101 fraction digits": {map[string]any{"xs": []any{1.0, jsonNumber("0." + strings.Repeat("1", 100))}}, "/xs/1", "digits"},
		"an exponent over 400":            {map[string]any{"n": jsonNumber("1e401")}, "/n", "exponent"},
		"an exponent under -400":          {map[string]any{"n": jsonNumber("-1E-401")}, "/n", "exponent"},
		"an exponent of many digits":      {map[string]any{"n": jsonNumber("1e999999999999999999999")}, "/n", "exponent"},
		// Go escapes U+2028 as \u2028, but the bound counts its 3 bytes of
		// UTF-8; a backslash and "u2028" as text are 7 bytes, escaped.
		"270 000 bytes of U+2028":    {map[string]any{"s": strings.Repeat("\u2028", 90_000)}, "", "270008 bytes"},
		"40 000 \\u2028 as text":     {map[string]any{"s": strings.Repeat("\\u2028", 40_000)}, "", "280008 bytes"},
		"80 000 U+2029, 30 000 x":    {map[string]any{"s": strings.Repeat("\u2029", 80_000) + strings.Repeat("x", 30_000)}, "", "270008 bytes"},
		"\\\\u2028 as text, escaped": {map[string]any{"s": strings.Repeat("\\\\u2028", 40_000)}, "", "360008 bytes"},
	} {
		within(t, name, func() {
			err := s.Validate(tc.body)
			if errors.Is(err, ErrValidationTooExpensive) {
				t.Fatalf("Validate(%s) = %v, want a body error", name, err)
			}
			got := problems(t, s, tc.body)
			if len(got) != 1 || got[0].Path != tc.path || !strings.Contains(got[0].Message, tc.says) {
				t.Errorf("Validate(%s): problems %v, want the one bound, at %q", name, got, tc.path)
			}
		})
	}
	// At the bounds, a body validates.
	accept := mustCompile(t, `{"properties":{"n":{"type":"number"}}}`)
	for name, body := range map[string]map[string]any{
		"10 000 values":             {"xs": numbers(maxBodyValues - 2)},
		"100 digits":                {"n": jsonNumber("1" + strings.Repeat("0", 99))},
		"an exponent of 400":        {"n": jsonNumber("1e400")},
		"an exponent of -400":       {"n": jsonNumber("-1.5e-400")},
		"256 KiB in HTML's letters": {"s": strings.Repeat("<>&", (maxBodyBytes-16)/3)},
		"135 000 bytes of U+2028":   {"s": strings.Repeat("\u2028", 45_000)},
		"256 KiB of U+2029":         {"s": strings.Repeat("\u2029", (maxBodyBytes-16)/3)},
	} {
		if err := accept.Validate(body); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", name, err)
		}
	}
}

// TestValidateTruncatesLongMessagesAndPaths cuts each message and each path to
// at most 512 bytes, at a rune boundary.
func TestValidateTruncatesLongMessagesAndPaths(t *testing.T) {
	// An enum of 100 values of 20 runes each, two bytes apiece.
	values := make([]string, 100)
	for i := range values {
		values[i] = strconv.Quote(strings.Repeat("é", 19) + string(rune('a'+i%26)))
	}
	long := strings.Repeat("é", 100_000) // 200 KB
	half := long[:len(long)/2]
	for name, tc := range map[string]struct {
		schema string
		body   map[string]any
		prefix string
	}{
		"an enum of 100 long values":                  {`{"properties":{"mode":{"enum":[` + strings.Join(values, ",") + `]}}}`, map[string]any{"mode": "none"}, "value must be one of"},
		"a long string that does not match a pattern": {`{"properties":{"s":{"pattern":"^x"}}}`, map[string]any{"s": long}, "'é"},
		"a long member name":                          {`{"additionalProperties":{"type":"string"}}`, map[string]any{long: 1.0}, "got number"},
		"long names not allowed":                      {`{"additionalProperties":false}`, map[string]any{half: 1.0, half + "x": 2.0}, "additional properties"},
	} {
		got := problems(t, mustCompile(t, tc.schema), tc.body)
		if len(got) != 1 {
			t.Errorf("%s: problems %v, want one", name, got)
			continue
		}
		p := got[0]
		for what, s := range map[string]string{"path": p.Path, "message": p.Message} {
			if len(s) > maxMessageBytes || !utf8.ValidString(s) {
				t.Errorf("%s: %s of %d bytes, want at most %d, valid UTF-8", name, what, len(s), maxMessageBytes)
			}
		}
		if !strings.HasPrefix(p.Message, tc.prefix) {
			t.Errorf("%s: message %q, want it to start with %q", name, p.Message, tc.prefix)
		}
	}
}

// TestValidateIsSafeForConcurrentUseOnHostileInput validates through one
// compiled schema from many goroutines, bodies that the budget stops and
// bodies with problems, as callers that cache compiled schemas do.
func TestValidateIsSafeForConcurrentUseOnHostileInput(t *testing.T) {
	expensive := mustCompile(t, branching(2))
	unique := mustCompile(t, `{"properties":{"xs":{"uniqueItems":true,"items":{"propertyNames":{"maxLength":3}}}}}`)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := expensive.Validate(chainBody(40)); !errors.Is(err, ErrValidationTooExpensive) {
				t.Errorf("Validate past the budget = %v, want ErrValidationTooExpensive", err)
			}
		})
		wg.Go(func() {
			for range 20 {
				var verr *ValidationError
				body := map[string]any{"xs": []any{map[string]any{"abcd": 1.0}, map[string]any{"abcd": 1.0}}}
				if err := unique.Validate(body); !errors.As(err, &verr) || len(verr.Problems) != 3 {
					t.Errorf("Validate = %v, want 3 problems", err)
					return
				}
			}
		})
	}
	wg.Wait()
}
