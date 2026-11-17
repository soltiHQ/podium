package workloadschema

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// regexLimit is how long Compile may take to refuse a regular expression past
// its bounds, without the race detector: it measures the expression before it
// compiles it. regexTimeLimit scales it for the race detector.
const regexLimit = 100 * time.Millisecond

func regexTimeLimit() time.Duration {
	if raceDetector {
		return regexLimit * raceSlowdown
	}
	return regexLimit
}

// refusedWith compiles each schema and fails the test unless Compile refuses
// it with an error that says what the case names.
func refusedWith(t *testing.T, cases map[string]struct{ schema, says string }) {
	t.Helper()
	for name, tc := range cases {
		within(t, name, func() {
			_, err := Compile([]byte(tc.schema))
			notPanicked(t, name, err)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("%s: Compile = %v, want an error that says %s", name, err, tc.says)
			}
		})
	}
}

// TestCompileRefusesWhatTheSubsetLeavesOut refuses each keyword of draft
// 2020-12 outside the subset, a $schema of another draft, and every $ref that
// does not resolve inside the schema, each by its name and location.
func TestCompileRefusesWhatTheSubsetLeavesOut(t *testing.T) {
	const applicator = `"https://json-schema.org/draft/2020-12/meta/applicator"`
	refusedWith(t, map[string]struct{ schema, says string }{
		"$dynamicRef":                      {`{"properties":{"a":{"$dynamicRef":"#n"}}}`, `"$dynamicRef" at "#/properties/a"`},
		"$dynamicAnchor":                   {`{"$dynamicAnchor":"n"}`, `"$dynamicAnchor" at "#"`},
		"$recursiveRef":                    {`{"allOf":[{"$recursiveRef":"#"}]}`, `"$recursiveRef" at "#/allOf/0"`},
		"$recursiveAnchor":                 {`{"$recursiveAnchor":true}`, `"$recursiveAnchor" at "#"`},
		"$vocabulary":                      {`{"$vocabulary":{"https://json-schema.org/draft/2020-12/vocab/core":true}}`, `"$vocabulary" at "#"`},
		"unevaluatedItems":                 {`{"items":{"unevaluatedItems":false}}`, `"unevaluatedItems" at "#/items"`},
		"unevaluatedProperties":            {`{"$defs":{"a":{"unevaluatedProperties":false}}}`, `"unevaluatedProperties" at "#/$defs/a"`},
		"a draft-07 $schema":               {`{"$schema":"http://json-schema.org/draft-07/schema#"}`, `"$schema" at "#"`},
		"a 2019-09 $schema":                {`{"$schema":"https://json-schema.org/draft/2019-09/schema"}`, `"$schema" at "#"`},
		"another 2020-12 $schema spelling": {`{"$schema":"https://json-schema.org/draft/2020-12/schema#"}`, `"$schema" at "#"`},
		"a $schema below the root":         {`{"properties":{"a":{"$schema":"http://json-schema.org/draft-07/schema#"}}}`, `"$schema" at "#/properties/a"`},
		"a $ref to the 2020-12 metaschema": {`{"$ref":"https://json-schema.org/draft/2020-12/schema"}`, "remote reference"},
		"a $ref to meta/applicator":        {`{"anyOf":[{"$ref":` + applicator + `}]}`, "remote reference"},
		"a relative $ref":                  {`{"$ref":"meta/applicator"}`, "remote reference"},
		"a relative $ref with a fragment":  {`{"$ref":"defs.json#/$defs/a"}`, "remote reference"},
		"an absolute $ref":                 {`{"$ref":"https://example.com/schema.json"}`, "remote reference"},
		"an empty $ref":                    {`{"$ref":""}`, "remote reference"},
		"an $id below the root":            {`{"$defs":{"a":{"$id":"a.json","type":"string"}}}`, `"$id" at "#/$defs/a"`},
		"an invalid pattern":               {`{"properties":{"a":{"pattern":"("}}}`, `"pattern" at "#/properties/a"`},
		"an invalid patternProperties key": {`{"patternProperties":{"(?<=a)b":true}}`, `"patternProperties" at "#"`},
		// The schemas of the round-2 findings: a root $dynamicAnchor that the
		// metaschemas' $dynamicRef would resolve to, and unevaluatedItems.
		"finding I1":                    {`{"$dynamicAnchor":"meta","anyOf":[{"$ref":` + applicator + `},{"$ref":` + applicator + `}]}`, `"$dynamicAnchor" at "#"`},
		"finding I1 without the anchor": {`{"anyOf":[{"$ref":` + applicator + `},{"$ref":` + applicator + `}]}`, "remote reference"},
		"finding 2": {
			`{"$defs":{"f":true},"patternProperties":{"x":{"unevaluatedItems":true,"$ref":"#/$defs/f"}}}`,
			`"unevaluatedItems" at "#/patternProperties/x"`,
		},
	})
}

// TestCompileRefusesMalformedKeywordValues refuses a keyword of the subset
// whose value is not what the keyword takes.
func TestCompileRefusesMalformedKeywordValues(t *testing.T) {
	refusedWith(t, map[string]struct{ schema, says string }{
		"type of a number":                {`{"type":5}`, `"type" at "#"`},
		"a type the subset has not":       {`{"type":"int"}`, `"type" at "#"`},
		"no type in a list":               {`{"type":[]}`, `"type" at "#"`},
		"a type twice":                    {`{"type":["string","string"]}`, `"type" at "#"`},
		"enum of a string":                {`{"enum":"a"}`, `"enum" at "#"`},
		"a negative minLength":            {`{"minLength":-1}`, `"minLength" at "#"`},
		"a fractional maxItems":           {`{"maxItems":1.5}`, `"maxItems" at "#"`},
		"minContains of a string":         {`{"minContains":"1"}`, `"minContains" at "#"`},
		"multipleOf zero":                 {`{"multipleOf":0}`, `"multipleOf" at "#"`},
		"a negative multipleOf":           {`{"multipleOf":-2}`, `"multipleOf" at "#"`},
		"multipleOf of a string":          {`{"multipleOf":"2"}`, `"multipleOf" at "#"`},
		"maximum of a boolean":            {`{"maximum":true}`, `"maximum" at "#"`},
		"exclusiveMinimum as draft-04":    {`{"exclusiveMinimum":true}`, `"exclusiveMinimum" at "#"`},
		"pattern of a number":             {`{"pattern":5}`, `"pattern" at "#"`},
		"uniqueItems of a string":         {`{"uniqueItems":"yes"}`, `"uniqueItems" at "#"`},
		"required of a string":            {`{"required":"a"}`, `"required" at "#"`},
		"a required name twice":           {`{"required":["a","a"]}`, `"required" at "#"`},
		"a required name of a number":     {`{"required":[1]}`, `"required" at "#"`},
		"dependentRequired of strings":    {`{"dependentRequired":{"a":"b"}}`, `"dependentRequired" at "#"`},
		"an empty allOf":                  {`{"allOf":[]}`, `"allOf" at "#"`},
		"anyOf of an object":              {`{"anyOf":{}}`, `"anyOf" at "#"`},
		"items of an array, as before":    {`{"items":[{"type":"string"}]}`, `"items" at "#"`},
		"properties of an array":          {`{"properties":[]}`, `"properties" at "#"`},
		"a subschema of a number":         {`{"not":5}`, `"not" at "#"`},
		"a property of a string":          {`{"properties":{"a":"string"}}`, `"#/properties/a"`},
		"$anchor that is not a name":      {`{"$anchor":"#a"}`, `"$anchor" at "#"`},
		"an $anchor twice":                {`{"$defs":{"a":{"$anchor":"x"},"b":{"$anchor":"x"}}}`, `"$anchor"`},
		"a $ref to no anchor":             {`{"$ref":"#nowhere"}`, `"$ref" at "#"`},
		"a $ref to nothing":               {`{"properties":{"a":{"$ref":"#/$defs/missing"}}}`, `"$ref" at "#/properties/a"`},
		"a $ref to a number":              {`{"x-defs":{"n":5},"$ref":"#/x-defs/n"}`, `"$ref" at "#"`},
		"a $defs entry of a number":       {`{"$defs":{"n":5}}`, `"#/$defs/n"`},
		"a $ref with a bad escape":        {`{"$defs":{"a":true},"$ref":"#/$defs/a~2"}`, `"$ref" at "#"`},
		"a $ref with bad percent-escapes": {`{"$ref":"#/%zz"}`, `"$ref" at "#"`},
		"a $ref of a number":              {`{"$ref":5}`, `"$ref" at "#"`},
		"title of a number":               {`{"title":5}`, `"title" at "#"`},
		"examples of a string":            {`{"examples":"a"}`, `"examples" at "#"`},
		"$id of a number":                 {`{"$id":5}`, `"$id" at "#"`},
	})
}

// TestCompileRefusesOversizedRegularExpressions measures each regular
// expression before it compiles it: 2 000 instructions at most for one, and
// 20 000 for all those of a schema. a{1000} is 1 002 instructions.
func TestCompileRefusesOversizedRegularExpressions(t *testing.T) {
	// nearLimit is a pattern of 1 900 instructions.
	const nearLimit = `"a{950}b{948}"`
	patterns := func(n int) string {
		props := make([]string, n)
		for i := range props {
			props[i] = `"p` + strconv.Itoa(i) + `":{"pattern":` + nearLimit + `}`
		}
		return `{"properties":{` + strings.Join(props, ",") + `}}`
	}
	for name, schema := range map[string]string{
		"a{1000} twice":                    `{"pattern":"a{1000}a{1000}"}`,
		"a{1000} 3 000 times, in 21 KB":    `{"pattern":"` + strings.Repeat("a{1000}", 3000) + `"}`,
		"a{1000} 9 000 times, in 63 KB":    `{"pattern":"` + strings.Repeat("a{1000}", 9000) + `"}`,
		"repeats of repeats":               `{"pattern":"((a{100}){100}){100}"}`,
		"a patternProperties key":          `{"patternProperties":{"` + strings.Repeat("a{1000}", 3) + `":true}}`,
		"11 of 1 900, over 20 000 in all":  patterns(11),
		"60 KB of literal characters":      `{"pattern":"` + strings.Repeat("a", 60_000) + `"}`,
		"a{1000} nested 1 000 levels deep": `{"pattern":"` + strings.Repeat("(", 1000) + "a" + strings.Repeat(")", 1000) + `{1000}"}`,
	} {
		withinBudget(t, name, regexTimeLimit(), func() {
			_, err := Compile([]byte(schema))
			if err == nil {
				t.Errorf("Compile(%s) succeeded, want the regular expression refused", name)
			}
		})
	}
	for name, schema := range map[string]string{
		"a{1000}":                        `{"pattern":"a{1000}"}`,
		"10 of 1 900, 19 000 in all":     patterns(10),
		"the same pattern in two places": `{"pattern":` + nearLimit + `,"patternProperties":{` + nearLimit + `:true}}`,
	} {
		if _, err := Compile([]byte(schema)); err != nil {
			t.Errorf("Compile(%s): %v", name, err)
		}
	}
}

// TestCompileRefusesASameLocationCycle refuses a cycle that comes back to a
// subschema at the same location of a body, through $ref and the keywords
// that apply a subschema to the value they sit at.
func TestCompileRefusesASameLocationCycle(t *testing.T) {
	for name, schema := range map[string]string{
		"a $ref to itself":      `{"$ref":"#"}`,
		"two $defs":             `{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}},"$ref":"#/$defs/a"}`,
		"through anyOf":         `{"$defs":{"a":{"anyOf":[{"type":"string"},{"$ref":"#/$defs/a"}]}},"$ref":"#/$defs/a"}`,
		"through oneOf":         `{"oneOf":[{"$ref":"#"}]}`,
		"through if and then":   `{"$defs":{"a":{"if":true,"then":{"$ref":"#/$defs/a"}}},"$ref":"#/$defs/a"}`,
		"through else":          `{"if":false,"else":{"$ref":"#"}}`,
		"through a dependency":  `{"dependentSchemas":{"x":{"$ref":"#"}}}`,
		"through not and allOf": `{"$defs":{"a":{"not":{"allOf":[{"$ref":"#/$defs/a"}]}}},"$ref":"#/$defs/a"}`,
		"through an anchor":     `{"$defs":{"a":{"$anchor":"a","allOf":[{"$ref":"#a"}]}}}`,
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
		"a linked list":   `{"properties":{"next":{"$ref":"#"}}}`,
		"a tree":          `{"properties":{"children":{"items":{"$ref":"#"}}}}`,
		"a JSON value":    `{"anyOf":[{"type":["null","boolean","number","string"]},{"type":"array","items":{"$ref":"#"}},{"type":"object","additionalProperties":{"$ref":"#"}}]}`,
		"property names":  `{"propertyNames":{"$ref":"#"}}`,
		"contains":        `{"contains":{"$ref":"#"}}`,
		"then without if": `{"then":{"$ref":"#"}}`,
	} {
		if _, err := Compile([]byte(schema)); err != nil {
			t.Errorf("Compile(%s): %v", name, err)
		}
	}
}

// TestCompileIgnoresWhatItDoesNotEvaluate leaves the values of keywords it
// does not know, and of annotations, uncompiled: whatever they hold, the
// schema compiles.
func TestCompileIgnoresWhatItDoesNotEvaluate(t *testing.T) {
	for name, schema := range map[string]string{
		"an unknown keyword holding a refused one": `{"x-defs":{"a":{"$dynamicRef":"#n"}},"foo":{"unevaluatedItems":false}}`,
		"default holding a refused one":            `{"default":{"$dynamicRef":"#n","$id":"x"}}`,
		"examples holding a remote $ref":           `{"examples":[{"$ref":"https://example.com/s.json"}]}`,
		"contentSchema holding a refused one":      `{"contentSchema":{"unevaluatedProperties":false}}`,
		"an enum holding an $id":                   `{"enum":[{"$id":"x","$schema":"y"}]}`,
		"a root $id":                               `{"$id":"https://example.com/schemas/deploy.json","type":"object"}`,
	} {
		if _, err := Compile([]byte(schema)); err != nil {
			t.Errorf("Compile(%s): %v", name, err)
		}
	}
}
