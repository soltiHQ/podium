package workloadschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// boundedBudget is how long the worst input within the bounds may take to
// compile or validate, and a large one to be refused. Either takes a few
// tenths of a second under the race detector, and several times that under
// the load of the whole suite or of other runs; a lost bound takes minutes.
// The tests tell a refusal from an overrun by its error, not by the clock.
const boundedBudget = 10 * time.Second

// wideDef is a schema that evaluates 1+refs·(width+2) subschemas at the body
// itself: an allOf of refs $refs to one definition, an allOf of width trues.
func wideDef(refs, width int) string {
	return `{"$defs":{"w":{"allOf":[` + strings.Repeat(`true,`, width-1) + `true]}},` +
		`"allOf":[` + strings.Repeat(`{"$ref":"#/$defs/w"},`, refs-1) + `{"$ref":"#/$defs/w"}]}`
}

// chainUnderItems applies anyOfChain(levels), a resource of its own, to each
// item of the member "xs".
func chainUnderItems(levels int) string {
	return `{"$defs":{"c":{"$id":"c.json",` + anyOfChain(levels)[1:] + `},` +
		`"properties":{"xs":{"items":{"$ref":"c.json"}}}}`
}

// decoded is a body as Validate decodes it.
func decoded(t *testing.T, body map[string]any) any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// chainBody is a body nested depth objects deep, each the member "a" of the
// one around it.
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

// mostItemsWithin is the most items of numbers under "xs" whose validation
// against s stays within the work bound.
func mostItemsWithin(t *testing.T, s *Schema) int {
	t.Helper()
	lo, hi := 0, maxBodyValues
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if s.plan.withinWork(decoded(t, map[string]any{"xs": numbers(mid)})) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func TestCompileRefusesFanOutPastTheBound(t *testing.T) {
	// anyOfChain(n) evaluates about 2^(n+2) subschemas at the body itself.
	for name, schema := range map[string]string{
		"an anyOf chain 8 deep":    anyOfChain(8),
		"an anyOf chain 169 deep":  anyOfChain(169),
		"31 refs to 31 subschemas": wideDef(31, 31),
	} {
		withinBudget(t, name, boundedBudget, func() {
			_, err := Compile([]byte(schema))
			if err == nil || !strings.Contains(err.Error(), "at one location") {
				t.Errorf("Compile(%s) = %v, want the fan-out refused", name, err)
			}
		})
	}
	for name, schema := range map[string]string{
		"an anyOf chain 7 deep":    anyOfChain(7),
		"30 refs to 31 subschemas": wideDef(30, 31),
	} {
		if _, err := Compile([]byte(schema)); err != nil {
			t.Errorf("Compile(%s): %v", name, err)
		}
	}
}

func TestCompileRefusesASameLocationCycle(t *testing.T) {
	for name, schema := range map[string]string{
		"a $ref to itself":       `{"$ref":"#"}`,
		"two $defs":              `{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}},"$ref":"#/$defs/a"}`,
		"through anyOf":          `{"$defs":{"a":{"anyOf":[{"type":"string"},{"$ref":"#/$defs/a"}]}},"$ref":"#/$defs/a"}`,
		"through if and then":    `{"$defs":{"a":{"if":true,"then":{"$ref":"#/$defs/a"}}},"$ref":"#/$defs/a"}`,
		"through a dependency":   `{"dependentSchemas":{"x":{"$ref":"#"}}}`,
		"through a $dynamicRef":  `{"$dynamicAnchor":"n","allOf":[{"$dynamicRef":"#n"}]}`,
		"through not and allOf":  `{"$defs":{"a":{"not":{"allOf":[{"$ref":"#/$defs/a"}]}}},"$ref":"#/$defs/a"}`,
		"through an older draft": `{"$schema":"http://json-schema.org/draft-07/schema#","definitions":{"a":{"$ref":"#/definitions/a"}},"allOf":[{"$ref":"#/definitions/a"}]}`,
	} {
		within(t, name, func() {
			_, err := Compile([]byte(schema))
			if err == nil || !strings.Contains(err.Error(), "cycle") {
				t.Errorf("Compile(%s) = %v, want the cycle refused", name, err)
			}
		})
	}
	// A reference back into the schema through a keyword that moves into the
	// body, as a recursive type is, is no cycle.
	for name, schema := range map[string]string{
		"a linked list": `{"properties":{"next":{"$ref":"#"}}}`,
		"a tree":        `{"properties":{"children":{"items":{"$ref":"#"}}}}`,
		"a JSON value":  `{"anyOf":[{"type":["null","boolean","number","string"]},{"type":"array","items":{"$ref":"#"}},{"type":"object","additionalProperties":{"$ref":"#"}}]}`,
	} {
		if _, err := Compile([]byte(schema)); err != nil {
			t.Errorf("Compile(%s): %v", name, err)
		}
	}
}

func TestCompileRefusesADynamicAnchorDeclaredTwice(t *testing.T) {
	schema := `{"$defs":{"a":{"$id":"a.json","$dynamicAnchor":"n","type":"string"},` +
		`"b":{"$id":"b.json","$dynamicAnchor":"n","type":"number"}},` +
		`"properties":{"x":{"$ref":"a.json"},"y":{"$dynamicRef":"#n"}},"$dynamicAnchor":"n"}`
	if _, err := Compile([]byte(schema)); err == nil || !strings.Contains(err.Error(), "$dynamicAnchor") {
		t.Errorf("Compile = %v, want the second $dynamicAnchor refused", err)
	}
}

// TestValidateAtTheBoundsStaysWithinTheBudget validates the worst a schema
// at the fan-out bound and a body at the body bounds can do, and a schema and
// body just within the work bound.
func TestValidateAtTheBoundsStaysWithinTheBudget(t *testing.T) {
	// 991 subschemas at the body itself, against a body of 10 000 values.
	s := mustCompile(t, wideDef(30, 31))
	body := map[string]any{"xs": numbers(maxBodyValues - 2)}
	if n := countValues(decoded(t, body), maxBodyValues+1); n != maxBodyValues {
		t.Fatalf("the body holds %d values, want %d", n, maxBodyValues)
	}
	start := time.Now()
	if err := s.Validate(body); err != nil {
		t.Errorf("Validate of a maximal body at the fan-out bound: %v", err)
	}
	t.Logf("fan-out 991, body of %d values: %v", maxBodyValues, time.Since(start).Round(time.Millisecond))

	// 510 subschemas at each item, as many items as the work bound allows.
	s = mustCompile(t, chainUnderItems(7))
	n := mostItemsWithin(t, s)
	if n == 0 || n >= maxBodyValues-2 {
		t.Fatalf("%d items within the work bound, want some and fewer than the body bound", n)
	}
	// It takes a few tenths of a second; under load, more than the budget of
	// 2 s may pass, which this test does not mean to measure.
	shortBudget(t, boundedBudget)
	start = time.Now()
	err := s.Validate(map[string]any{"xs": numbers(n)})
	took := time.Since(start)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Errorf("Validate at the work bound = %v, want the items' problems", err)
	}
	if took > boundedBudget {
		t.Errorf("Validate at the work bound took %v, want at most %v", took, boundedBudget)
	}
	t.Logf("fan-out 510 at each of %d items, at the work bound: %v", n, took.Round(time.Millisecond))
}

func TestValidateRefusesABodyPastItsBounds(t *testing.T) {
	// A body that validation would find a problem with: only the bound shows.
	s := mustCompile(t, `{"required":["missing"]}`)
	for name, tc := range map[string]struct {
		body map[string]any
		says string
	}{
		"over 256 KiB":       {map[string]any{"s": strings.Repeat("x", maxBodyBytes)}, "bytes"},
		"over 10 000 values": {map[string]any{"xs": numbers(maxBodyValues)}, "values"},
	} {
		withinBudget(t, name, boundedBudget, func() {
			err := s.Validate(tc.body)
			if errors.Is(err, ErrValidationTooExpensive) {
				t.Fatalf("Validate(%s) = %v, want a body error", name, err)
			}
			got := problems(t, s, tc.body)
			if len(got) != 1 || got[0].Path != "" || !strings.Contains(got[0].Message, tc.says) {
				t.Errorf("Validate(%s): problems %v, want the one bound, at the body", name, got)
			}
		})
	}
}

// TestValidateRefusesWorkPastTheBound validates bodies that the fan-out bound
// and the body bounds both let through: a schema that applies two subschemas
// to the same member multiplies its work level after level, and a deep body
// that fails at each level makes the validator copy the path at each. Both
// are refused before the validator runs, at once.
func TestValidateRefusesWorkPastTheBound(t *testing.T) {
	branching := mustCompile(t, `{"anyOf":[`+
		`{"properties":{"a":{"$ref":"#"}},"required":["a"]},`+
		`{"properties":{"a":{"$ref":"#"}},"required":["a"]}]}`)
	recursive := mustCompile(t, `{"properties":{"a":{"$ref":"#"}},"required":["b"]}`)
	for name, tc := range map[string]struct {
		s    *Schema
		body map[string]any
	}{
		"two branches into one member, 40 levels": {branching, chainBody(40)},
		"a failure at each of 9 000 levels":       {recursive, chainBody(9_000)},
	} {
		withinBudget(t, name, boundedBudget, func() {
			// The walk refuses in steps; the budget of 2 s would end the
			// validator itself, which the bound means never to run.
			err := tc.s.Validate(tc.body)
			if !errors.Is(err, ErrValidationTooExpensive) || !strings.Contains(err.Error(), "steps") {
				t.Errorf("Validate(%s) = %v, want ErrValidationTooExpensive from the walk", name, err)
			}
		})
	}
	// The same schemas on bodies of a usual depth validate as they did.
	if got := problems(t, recursive, chainBody(3)); len(got) != 4 || got[3].Path != "/a/a/a" {
		t.Errorf("problems %v, want 'b' missing at each of the 4 levels", got)
	}
	if err := branching.Validate(map[string]any{"a": map[string]any{"a": 1}}); err != nil {
		t.Errorf("Validate of a short chain: %v", err)
	}
}

// TestValidateAfterAnOverrunValidatesAfresh overruns a budget of a
// millisecond, then validates the same body against the same schema under the
// usual budget: nothing of the overrun remains.
func TestValidateAfterAnOverrunValidatesAfresh(t *testing.T) {
	s := mustCompile(t, chainUnderItems(7))
	body := map[string]any{"xs": numbers(mostItemsWithin(t, s) / 2)}
	before := runtime.NumGoroutine()

	shortBudget(t, time.Millisecond)
	err := s.Validate(body)
	if !errors.Is(err, ErrValidationTooExpensive) || !strings.Contains(err.Error(), "budget of 1ms") {
		t.Fatalf("Validate under a budget of 1ms = %v, want the overrun", err)
	}

	validationBudget = boundedBudget
	var verr *ValidationError
	if err := s.Validate(body); !errors.As(err, &verr) {
		t.Errorf("Validate after the overrun = %v, want the items' problems", err)
	}

	// The abandoned validator ends on its own.
	deadline := time.Now().Add(boundedBudget)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("the abandoned validator still runs after %v", boundedBudget)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestValidateTruncatesLongMessages(t *testing.T) {
	// An enum of 100 values of 20 runes each, two bytes apiece.
	values := make([]string, 100)
	for i := range values {
		values[i] = strconv.Quote(strings.Repeat("é", 19) + string(rune('a'+i%26)))
	}
	s := mustCompile(t, `{"properties":{"mode":{"enum":[`+strings.Join(values, ",")+`]}}}`)
	got := problems(t, s, map[string]any{"mode": "none"})
	if len(got) != 1 {
		t.Fatalf("problems %v, want one", got)
	}
	m := got[0].Message
	if len(m) > maxMessageBytes || !utf8.ValidString(m) || !strings.HasSuffix(m, "…") || !strings.HasPrefix(m, "value must be one of") {
		t.Errorf("message of %d bytes %q, want the enum message cut to at most %d bytes, valid UTF-8, ending in …", len(m), m, maxMessageBytes)
	}
}
