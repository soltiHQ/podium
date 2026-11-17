package workloadschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// hostileBudget is how long compiling or validating hostile input may take:
// a schema an agent sends must not hold the sync that carries it any longer.
const hostileBudget = time.Second

// deploySchema is the schema the validation tests check bodies against.
const deploySchema = `{
	"type": "object",
	"required": ["name"],
	"properties": {
		"name": {"type": "string"},
		"mode": {"enum": ["fast", "safe"]},
		"count": {"type": "integer", "minimum": 1},
		"a/b~c": {"type": "boolean"},
		"ports": {"type": "array", "items": {"type": "integer", "minimum": 1}},
		"env": {"type": "object", "additionalProperties": {"type": "string"}}
	}
}`

// padded is s followed by spaces up to n bytes: JSON of exactly n bytes.
func padded(s string, n int) []byte {
	return []byte(s + strings.Repeat(" ", n-len(s)))
}

// mustCompile compiles a schema the test relies on.
func mustCompile(t *testing.T, schema string) *Schema {
	t.Helper()
	s, err := Compile([]byte(schema))
	if err != nil {
		t.Fatalf("Compile(%s): %v", schema, err)
	}
	return s
}

// problems is what Validate finds wrong with body. It fails the test unless
// Validate returns a *ValidationError.
func problems(t *testing.T, s *Schema, body map[string]any) []Problem {
	t.Helper()
	err := s.Validate(body)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Validate = %v (%T), want a *ValidationError", err, err)
	}
	return verr.Problems
}

// closedAddr is a local address nobody listens on. A fetch from it fails at
// once with "connection refused", which is not the loader's refusal.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// within runs f and fails the test when f takes longer than hostileBudget.
func within(t *testing.T, name string, f func()) {
	t.Helper()
	withinBudget(t, name, hostileBudget, f)
}

// withinBudget runs f and fails the test when f takes longer than budget.
func withinBudget(t *testing.T, name string, budget time.Duration, f func()) {
	t.Helper()
	start := time.Now()
	f()
	if took := time.Since(start); took > budget {
		t.Errorf("%s took %v, want at most %v", name, took.Round(time.Millisecond), budget)
	}
}

// notPanicked fails the test when err is a panic of the compiler that
// Compile recovered: hostile input must be refused, not crash the compiler.
func notPanicked(t *testing.T, name string, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "panicked") {
		t.Errorf("%s: %v", name, err)
	}
}

func TestFingerprintIgnoresKeyOrderAndWhitespace(t *testing.T) {
	compact := `{"type":"object","required":["name"],"properties":{"name":{"type":"string"},"replicas":{"type":"integer","minimum":1}}}`
	indented := "{\n" +
		"  \"properties\": {\n" +
		"    \"replicas\": { \"minimum\": 1, \"type\": \"integer\" },\n" +
		"    \"name\": {\n\t\t\"type\": \"string\"\n    }\n" +
		"  },\r\n" +
		"  \"required\": [ \"name\" ],\n" +
		"  \"type\": \"object\"\n" +
		"}\n"
	want, err := Fingerprint([]byte(compact))
	if err != nil {
		t.Fatalf("Fingerprint(compact): %v", err)
	}
	got, err := Fingerprint([]byte(indented))
	if err != nil {
		t.Fatalf("Fingerprint(indented): %v", err)
	}
	if got != want {
		t.Fatalf("Fingerprint(indented) = %s, want %s, as for the compact form", got, want)
	}
	// A schema with one value changed is another schema.
	other, err := Fingerprint([]byte(strings.Replace(compact, `"minimum":1`, `"minimum":2`, 1)))
	if err != nil {
		t.Fatalf("Fingerprint(other): %v", err)
	}
	if other == want {
		t.Fatalf("a schema with another minimum has the same fingerprint %s", want)
	}
}

func TestFingerprintIsSha256OfTheCanonicalForm(t *testing.T) {
	sum := sha256.Sum256([]byte(`{"type":"object"}`))
	want := hex.EncodeToString(sum[:])
	for _, raw := range []string{`{"type":"object"}`, "{ \"type\" :\t\"object\" }\n"} {
		got, err := Fingerprint([]byte(raw))
		if err != nil {
			t.Fatalf("Fingerprint(%q): %v", raw, err)
		}
		if got != want {
			t.Errorf("Fingerprint(%q) = %s, want %s", raw, got, want)
		}
	}
}

// TestFingerprintFollowsRFC8785 holds the fingerprint to the examples of RFC
// 8785 itself, the canonical form the agents' SDK computes it over as well:
// numbers in their ECMAScript form, strings with the fewest escapes, and keys
// sorted by their UTF-16 code units, which puts U+1F600 before U+FB33.
func TestFingerprintFollowsRFC8785(t *testing.T) {
	for name, tc := range map[string]struct{ input, canonical string }{
		"section 3.2.2, primitive values": {
			input: `{
  "numbers": [333333333.33333329, 1E30, 4.50,
              2e-3, 0.000000000000000000000000001],
  "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
  "literals": [null, true, false]
}`,
			// The UTF-8 of the canonical form, as section 3.2.4 gives it in hex.
			canonical: unhex(t, `
				7b 22 6c 69 74 65 72 61 6c 73 22 3a 5b 6e 75 6c 6c 2c 74 72
				75 65 2c 66 61 6c 73 65 5d 2c 22 6e 75 6d 62 65 72 73 22 3a
				5b 33 33 33 33 33 33 33 33 33 2e 33 33 33 33 33 33 33 2c 31
				65 2b 33 30 2c 34 2e 35 2c 30 2e 30 30 32 2c 31 65 2d 32 37
				5d 2c 22 73 74 72 69 6e 67 22 3a 22 e2 82 ac 24 5c 75 30 30
				30 66 5c 6e 41 27 42 5c 22 5c 5c 5c 5c 5c 22 2f 22 7d`),
		},
		"section 3.2.3, sorting of properties": {
			input: `{
  "\u20ac": "Euro Sign",
  "\r": "Carriage Return",
  "\ufb33": "Hebrew Letter Dalet With Dagesh",
  "1": "One",
  "\ud83d\ude00": "Emoji: Grinning Face",
  "\u0080": "Control",
  "\u00f6": "Latin Small Letter O With Diaeresis"
}`,
			// The values in the order the RFC lists them after sorting.
			canonical: "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\"," +
				"\"\u00f6\":\"Latin Small Letter O With Diaeresis\",\"\u20ac\":\"Euro Sign\"," +
				"\"\U0001F600\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}",
		},
	} {
		sum := sha256.Sum256([]byte(tc.canonical))
		want := hex.EncodeToString(sum[:])
		got, err := Fingerprint([]byte(tc.input))
		if err != nil {
			t.Errorf("%s: Fingerprint: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%s: Fingerprint = %s, want %s, the sha256 of %s", name, got, want, tc.canonical)
		}
	}
}

// unhex decodes hex written in groups and lines.
func unhex(t *testing.T, s string) string {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// refused is input that is not a schema: over the limit, not JSON, or JSON
// that is not an object. A duplicate key is not JSON that RFC 8785 accepts.
func refused() map[string][]byte {
	return map[string][]byte{
		"one byte over the limit": padded(`{"type":"object"}`, MaxSchemaBytes+1),
		"an array":                []byte(`[]`),
		"a string":                []byte(`"x"`),
		"not JSON":                []byte(`{"type":`),
		"a duplicate key":         []byte(`{"type":"string","type":"object"}`),
	}
}

func TestFingerprintRefusesOversizedOrNonObject(t *testing.T) {
	if _, err := Fingerprint(padded(`{"type":"object"}`, MaxSchemaBytes)); err != nil {
		t.Fatalf("Fingerprint of a schema at the limit: %v", err)
	}
	for name, raw := range refused() {
		if fp, err := Fingerprint(raw); err == nil {
			t.Errorf("Fingerprint(%s) = %s, want an error", name, fp)
		}
	}
}

func TestCompileRefusesOversizedOrNonObject(t *testing.T) {
	if _, err := Compile(padded(`{"type":"object"}`, MaxSchemaBytes)); err != nil {
		t.Fatalf("Compile of a schema at the limit: %v", err)
	}
	for name, raw := range refused() {
		if _, err := Compile(raw); err == nil {
			t.Errorf("Compile(%s) succeeded, want an error", name)
		}
	}
}

func TestCompileRefusesRemoteAndFileRefs(t *testing.T) {
	closed := closedAddr(t)
	// A schema that a file loader would read and compile, if there were one.
	file := filepath.Join(t.TempDir(), "defs.json")
	if err := os.WriteFile(file, []byte(`{"type":"string"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, schema := range map[string]string{
		"https $ref":            `{"$ref":"https://` + closed + `/defs.json"}`,
		"http $ref":             `{"$ref":"http://` + closed + `/defs.json"}`,
		"file $ref":             `{"$ref":"file:///etc/passwd"}`,
		"file $ref to a schema": `{"$ref":"file://` + filepath.ToSlash(file) + `"}`,
		"relative $ref":         `{"$id":"https://` + closed + `/root.json","$ref":"defs.json"}`,
		"nested $ref":           `{"properties":{"name":{"$ref":"https://` + closed + `/defs.json#/$defs/name"}}}`,
	} {
		within(t, name, func() {
			_, err := Compile([]byte(schema))
			if err == nil || !strings.Contains(err.Error(), "remote reference") {
				t.Errorf("Compile(%s) = %v, want a remote reference error", name, err)
			}
		})
	}
	// A $dynamicRef, a $schema of another draft and an $id below the root are
	// refused for what they are, before any of them could name a URL
	// (TestCompileRefusesWhatTheSubsetLeavesOut has more).
	for name, tc := range map[string]struct{ schema, says string }{
		"$dynamicRef":  {`{"$dynamicRef":"https://` + closed + `/defs.json#node"}`, `"$dynamicRef" at "#"`},
		"$schema":      {`{"$schema":"https://` + closed + `/meta.json"}`, `"$schema" at "#"`},
		"embedded $id": {`{"$id":"https://` + closed + `/root.json","$defs":{"port":{"$id":"port.json","type":"integer"}},"properties":{"port":{"$ref":"port.json"}}}`, `"$id" at "#/$defs/port"`},
	} {
		within(t, name, func() {
			_, err := Compile([]byte(tc.schema))
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("Compile(%s) = %v, want an error that says %s", name, err, tc.says)
			}
		})
	}
	// A reference inside the document resolves: by pointer or by anchor.
	for name, schema := range map[string]string{
		"pointer": `{"$defs":{"port":{"type":"integer"}},"properties":{"port":{"$ref":"#/$defs/port"}}}`,
		"anchor":  `{"$defs":{"port":{"$anchor":"port","type":"integer"}},"properties":{"port":{"$ref":"#port"}}}`,
	} {
		s, err := Compile([]byte(schema))
		if err != nil {
			t.Errorf("Compile(%s): %v", name, err)
			continue
		}
		got := problems(t, s, map[string]any{"port": "eighty"})
		if want := []Problem{{Path: "/port", Message: "got string, want integer"}}; !slices.Equal(got, want) {
			t.Errorf("%s: problems %v, want %v", name, got, want)
		}
	}
}

func TestCompileHandlesARefCycleAndDeepNesting(t *testing.T) {
	// Arrays nest at two bytes a level: the deepest nesting that fits the
	// limit, over three times as deep as the JSON decoders go.
	levels := (MaxSchemaBytes - len(`{"enum":[]}`)) / 2
	arrays := `{"enum":[` + strings.Repeat("[", levels) + strings.Repeat("]", levels) + `]}`
	if len(arrays) > MaxSchemaBytes || len(arrays) < MaxSchemaBytes-1 {
		t.Fatalf("nested arrays are %d bytes, want the most that fits %d", len(arrays), MaxSchemaBytes)
	}
	within(t, "nested arrays at the limit", func() {
		_, err := Compile([]byte(arrays))
		notPanicked(t, "nested arrays at the limit", err)
		if err == nil {
			t.Errorf("Compile of %d nested arrays succeeded, want an error", levels)
		}
	})

	// A cycle at one location of the body is refused, before any body meets
	// it (TestCompileRefusesASameLocationCycle has more of them).
	for name, schema := range map[string]string{
		"a $ref cycle":     `{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}},"$ref":"#/$defs/a"}`,
		"a $ref to itself": `{"$ref":"#"}`,
	} {
		within(t, name, func() {
			_, err := Compile([]byte(schema))
			notPanicked(t, name, err)
			if err == nil || !strings.Contains(err.Error(), "cycle") {
				t.Errorf("Compile(%s) = %v, want the cycle refused", name, err)
			}
		})
	}
	// A $ref that recurses into the body is no cycle: it validates a
	// linked list, one level of the schema for each level of the body.
	body := map[string]any{"next": map[string]any{"next": map[string]any{}}}
	s := mustCompile(t, `{"properties":{"next":{"$ref":"#"}},"additionalProperties":false}`)
	if err := s.Validate(body); err != nil {
		t.Errorf("Validate of a linked list: %v", err)
	}
}

// TestCompileBoundsTheWidestAndDeepestSchemas holds Compile to its two bounds
// on a schema's shape, far within 64 KiB: many subschemas, and deep nesting.
// It refuses a schema one past either bound at once, the 64 KiB ones
// included, and compiles and validates against the worst schemas within both
// bounds inside hostileBudget.
func TestCompileBoundsTheWidestAndDeepestSchemas(t *testing.T) {
	// wide is n values under allOf, with the root object one more.
	wide := func(n int, value string) string {
		return `{"allOf":[` + strings.Repeat(value+`,`, n-1) + value + `]}`
	}
	// chain is levels objects, each the "if" of the one around it.
	chain := func(levels int) string {
		return strings.Repeat(`{"if":`, levels) + `true` + strings.Repeat(`}`, levels)
	}
	widest := (MaxSchemaBytes-len(`{"allOf":[{}]}`))/len(`{},`) + 1
	deepest := (MaxSchemaBytes - len(`true`)) / len(`{"if":}`)
	for name, tc := range map[string]struct{ schema, refusal string }{
		"64 KiB of subschemas":       {wide(widest, `{}`), "objects and booleans"},
		"64 KiB of nesting":          {chain(deepest), "nests deeper"},
		"one object past the bound":  {wide(maxSchemaValues, `{}`), "objects and booleans"},
		"one boolean past the bound": {wide(maxSchemaValues, `true`), "objects and booleans"},
		"one level past the bound":   {chain(maxSchemaDepth + 1), "nests deeper"},
	} {
		if len(tc.schema) > MaxSchemaBytes {
			t.Fatalf("%s: %d bytes, over MaxSchemaBytes", name, len(tc.schema))
		}
		within(t, name, func() {
			_, err := Compile([]byte(tc.schema))
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Errorf("Compile(%s) = %v, want an error that says %q", name, err, tc.refusal)
			}
		})
	}

	// Chains as deep as the bound allows under allOf and its array, as many
	// as the bound on values allows.
	levels := maxSchemaDepth - 2
	parts := make([]string, (maxSchemaValues-1)/(levels+1))
	for i := range parts {
		parts[i] = chain(levels)
	}
	for name, schema := range map[string]string{
		"the most objects":     wide(maxSchemaValues-1, `{}`),
		"the most booleans":    wide(maxSchemaValues-1, `true`),
		"the deepest chain":    chain(maxSchemaDepth),
		"the most such chains": `{"allOf":[` + strings.Join(parts, ",") + `]}`,
	} {
		within(t, name, func() {
			s, err := Compile([]byte(schema))
			if err != nil {
				t.Errorf("Compile(%s): %v", name, err)
				return
			}
			if err := s.Validate(map[string]any{"a": "b"}); err != nil {
				t.Errorf("Validate against %s: %v", name, err)
			}
		})
	}
}

func TestCompileDefaultsToDraft2020(t *testing.T) {
	// prefixItems is a keyword of draft 2020-12, which draft-07 ignores.
	const items = `"properties":{"args":{"prefixItems":[{"type":"string"}]}}`
	body := map[string]any{"args": []any{float64(1)}}
	want := []Problem{{Path: "/args/0", Message: "got number, want string"}}
	for name, schema := range map[string]string{
		"no $schema":      `{` + items + `}`,
		"draft 2020-12's": `{"$schema":"https://json-schema.org/draft/2020-12/schema",` + items + `}`,
	} {
		if got := problems(t, mustCompile(t, schema), body); !slices.Equal(got, want) {
			t.Errorf("%s: problems %v, want %v", name, got, want)
		}
	}
	// A schema that names another draft is refused, not compiled as that
	// draft.
	if _, err := Compile([]byte(`{"$schema":"http://json-schema.org/draft-07/schema#",` + items + `}`)); err == nil || !strings.Contains(err.Error(), `"$schema" at "#"`) {
		t.Errorf("Compile of a draft-07 schema = %v, want its $schema refused", err)
	}
}

func TestValidateReportsPathsAndCapsProblems(t *testing.T) {
	s := mustCompile(t, deploySchema)
	ports := make([]any, 25)
	for i := range ports {
		ports[i] = float64(0)
	}
	// name is missing, mode is not in its enum, count and a/b~c have the
	// wrong type, and all 25 ports are under their minimum: 29 problems.
	got := problems(t, s, map[string]any{"mode": "slow", "count": "three", "a/b~c": float64(1), "ports": ports})
	want := []Problem{
		{Path: "", Message: "missing property 'name'"},
		{Path: "/a~1b~0c", Message: "got number, want boolean"},
		{Path: "/count", Message: "got string, want integer"},
		{Path: "/mode", Message: "value must be one of 'fast', 'safe'"},
	}
	for i := 0; len(want) < 20; i++ {
		want = append(want, Problem{Path: "/ports/" + strconv.Itoa(i), Message: "minimum: got 0, want 1"})
	}
	if !slices.Equal(got, want) {
		t.Fatalf("problems:\n%v\nwant the first 20 by path:\n%v", got, want)
	}
}

func TestValidateAcceptsAValidBody(t *testing.T) {
	s := mustCompile(t, deploySchema)
	for name, body := range map[string]map[string]any{
		"as REST decodes it": {
			"name": "web", "mode": "fast", "count": float64(3), "a/b~c": true,
			"ports": []any{float64(80), float64(443)}, "env": map[string]any{"LANG": "C"},
		},
		"as Go code builds it": {
			"name": "web", "count": 3, "ports": []int{80, 443}, "env": map[string]string{"LANG": "C"},
		},
	} {
		if err := s.Validate(body); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", name, err)
		}
	}
	// A nil body is the empty object.
	if err := mustCompile(t, `{"type":"object"}`).Validate(nil); err != nil {
		t.Errorf("Validate(nil) = %v, want nil", err)
	}
}

// TestValidateReportsABodyItCannotEncode holds Validate to its contract, nil
// or a *ValidationError, for a body that is not JSON at all.
func TestValidateReportsABodyItCannotEncode(t *testing.T) {
	s := mustCompile(t, `{"type":"object"}`)
	deep := map[string]any{}
	for range 10_000 {
		deep = map[string]any{"a": deep}
	}
	for name, body := range map[string]map[string]any{
		"a NaN": {"ratio": math.NaN()},
		"nested past the depth the decoder accepts": deep,
	} {
		got := problems(t, s, body)
		if len(got) != 1 || got[0].Path != "" {
			t.Errorf("%s: problems %v, want one, at the body", name, got)
		}
	}
}

// TestValidateIsSafeForConcurrentUse validates through one compiled schema
// from many goroutines, as callers that cache compiled schemas do.
func TestValidateIsSafeForConcurrentUse(t *testing.T) {
	s := mustCompile(t, deploySchema)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if err := s.Validate(map[string]any{"name": "web", "ports": []any{float64(80)}}); err != nil {
					t.Errorf("Validate of a valid body: %v", err)
					return
				}
				var verr *ValidationError
				if err := s.Validate(map[string]any{"ports": []any{"80"}}); !errors.As(err, &verr) || len(verr.Problems) != 2 {
					t.Errorf("Validate of an invalid body = %v, want 2 problems", err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// anyOfChain is levels definitions, each an anyOf of two $refs to the next,
// down to false: evaluating it takes 2^levels steps on any body.
func anyOfChain(levels int) string {
	var b strings.Builder
	b.WriteString(`{"$defs":{`)
	for i := range levels {
		fmt.Fprintf(&b, `"d%d":{"anyOf":[{"$ref":"#/$defs/d%d"},{"$ref":"#/$defs/d%d"}]},`, i, i+1, i+1)
	}
	fmt.Fprintf(&b, `"d%d":false},"$ref":"#/$defs/d0"}`, levels)
	return b.String()
}
