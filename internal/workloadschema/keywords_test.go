package workloadschema

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// obj and arr are JSON objects and arrays as REST decodes them.
type (
	obj = map[string]any
	arr = []any
)

// jsonNumber is a number as a caller that decodes with UseNumber holds it,
// spelled exactly.
func jsonNumber(s string) json.Number { return json.Number(s) }

// atMember validates v as the member "v" of a body, against a schema that
// applies sub to that member.
func atMember(t *testing.T, sub string, v any) error {
	t.Helper()
	s, err := Compile([]byte(`{"properties":{"v":` + sub + `}}`))
	if err != nil {
		t.Fatalf("Compile(%s): %v", sub, err)
	}
	return s.Validate(obj{"v": v})
}

// TestEachKeywordAcceptsAndRejects holds each keyword of the subset to at
// least one value it accepts and one it rejects.
func TestEachKeywordAcceptsAndRejects(t *testing.T) {
	hundred := make([]string, 100)
	for i := range hundred {
		hundred[i] = strconv.Itoa(i)
	}
	for _, tc := range []struct {
		name    string
		schema  string
		valid   []any
		invalid []any
	}{
		{"type string", `{"type":"string"}`, arr{"", "x"}, arr{1.0, true, nil, arr{}, obj{}}},
		{"type number", `{"type":"number"}`, arr{1.0, 1.5, jsonNumber("-1e-300")}, arr{"1", nil}},
		{
			"type integer accepts a number with no fraction", `{"type":"integer"}`,
			arr{1.0, jsonNumber("1.0"), jsonNumber("-20e-1"), jsonNumber("1e2"), jsonNumber("0.0")},
			arr{1.5, jsonNumber("1.000000000000000000001"), jsonNumber("1e-2"), "1"},
		},
		{"type boolean", `{"type":"boolean"}`, arr{true, false}, arr{0.0, "true", nil}},
		{"type null", `{"type":"null"}`, arr{nil}, arr{false, 0.0, ""}},
		{"type array", `{"type":"array"}`, arr{arr{}, arr{1.0}}, arr{obj{}, "[]"}},
		{"type object", `{"type":"object"}`, arr{obj{}, obj{"a": 1.0}}, arr{arr{}, nil}},
		{"type of several names", `{"type":["string","null"]}`, arr{"x", nil}, arr{1.0, false}},
		{
			"enum compares JSON values", `{"enum":[1,"a",null,{"x":[1,2]},[{"y":true}]]}`,
			arr{1.0, jsonNumber("1.0"), jsonNumber("10e-1"), "a", nil, obj{"x": arr{1.0, 2.0}}, arr{obj{"y": true}}},
			arr{2.0, "b", false, obj{"x": arr{2.0, 1.0}}, obj{"x": arr{1.0, 2.0}, "z": nil}, arr{obj{"y": false}}, arr{}},
		},
		{
			"enum of many values", `{"enum":[` + strings.Join(hundred, ",") + `]}`,
			arr{0.0, 42.0, jsonNumber("99.0")},
			arr{100.0, 0.5, "1"},
		},
		{"enum of none", `{"enum":[]}`, nil, arr{1.0, nil}},
		{
			"const compares JSON values", `{"const":{"a":[1,{"b":null}],"c":"d"}}`,
			arr{obj{"c": "d", "a": arr{jsonNumber("1.0"), obj{"b": nil}}}},
			arr{obj{"a": arr{1.0, obj{"b": nil}}}, obj{"c": "d", "a": arr{1.0, obj{"b": false}}}},
		},
		{"const of a scalar", `{"const":"x"}`, arr{"x"}, arr{"y", nil}},
		{
			"multipleOf a decimal, exactly", `{"multipleOf":0.01}`,
			arr{0.07, 19.99, jsonNumber("1234567.89"), 0.0, -0.05, 3.0},
			arr{0.001, jsonNumber("0.015"), 1.005},
		},
		{
			"multipleOf an integer", `{"multipleOf":7}`,
			arr{14.0, -21.0, jsonNumber("7e300"), jsonNumber("700000000000000000000000000000000000007")},
			arr{15.0, 3.5, jsonNumber("700000000000000000000000000000000000001")},
		},
		{"multipleOf reads only numbers", `{"multipleOf":2}`, arr{"3", true}, nil},
		{"maximum", `{"maximum":10}`, arr{10.0, 9.5, -1e300, "11"}, arr{10.5, jsonNumber("10.000000000000000000000000001")}},
		{"exclusiveMaximum", `{"exclusiveMaximum":10}`, arr{9.99, jsonNumber("9.999999999999999999999999")}, arr{10.0, 11.0}},
		{"minimum", `{"minimum":-1.5}`, arr{-1.5, 0.0}, arr{-1.6, jsonNumber("-1.50000000000000000000001")}},
		{"exclusiveMinimum", `{"exclusiveMinimum":0}`, arr{jsonNumber("1e-300")}, arr{0.0, -1.0}},
		{"maxLength counts code points", `{"maxLength":3}`, arr{"abc", "日本語", ""}, arr{"abcd", "日本語!"}},
		{"minLength counts code points", `{"minLength":3}`, arr{"日本語", "abc", 1.0}, arr{"日本", "ab"}},
		{"pattern searches the string", `{"pattern":"b+c"}`, arr{"abbc", "bc", 1.0}, arr{"ac", ""}},
		{"pattern anchored", `{"pattern":"^[a-z0-9-]+$"}`, arr{"web-1"}, arr{"Web", "web_1", ""}},
		{"maxItems", `{"maxItems":2}`, arr{arr{}, arr{1.0, 2.0}, "abc"}, arr{arr{1.0, 2.0, 3.0}}},
		{"minItems", `{"minItems":1}`, arr{arr{nil}, obj{}}, arr{arr{}}},
		{
			"uniqueItems counts 1 and 1.0 alike", `{"uniqueItems":true}`,
			arr{arr{}, arr{1.0, 2.0}, arr{obj{"a": 1.0}, obj{"a": 2.0}}, arr{arr{1.0}, arr{1.0, 1.0}}},
			arr{arr{1.0, jsonNumber("1.0")}, arr{obj{"a": 1.0, "b": 2.0}, obj{"b": 2.0, "a": 1.0}}, arr{"x", 1.0, "x"}},
		},
		{"uniqueItems false", `{"uniqueItems":false}`, arr{arr{1.0, 1.0}}, nil},
		{"contains", `{"contains":{"type":"string"}}`, arr{arr{1.0, "a"}, obj{}}, arr{arr{}, arr{1.0, true}}},
		{"minContains", `{"contains":{"type":"string"},"minContains":2}`, arr{arr{"a", 1.0, "b"}}, arr{arr{"a", 1.0}}},
		{"maxContains", `{"contains":{"type":"string"},"maxContains":1}`, arr{arr{"a", 1.0}}, arr{arr{"a", "b"}, arr{1.0}}},
		{"minContains 0", `{"contains":{"type":"string"},"minContains":0,"maxContains":1}`, arr{arr{}, arr{1.0}, arr{"a"}}, arr{arr{"a", "b"}}},
		{"minContains and maxContains without contains", `{"minContains":2,"maxContains":0}`, arr{arr{}, arr{1.0}}, nil},
		{"maxProperties", `{"maxProperties":1}`, arr{obj{}, obj{"a": 1.0}}, arr{obj{"a": 1.0, "b": 2.0}}},
		{"minProperties", `{"minProperties":1}`, arr{obj{"a": nil}}, arr{obj{}}},
		{"required", `{"required":["a","b"]}`, arr{obj{"a": 1.0, "b": nil}, "not an object"}, arr{obj{"a": 1.0}, obj{}}},
		{
			"dependentRequired", `{"dependentRequired":{"a":["b","c"]}}`,
			arr{obj{}, obj{"b": 1.0}, obj{"a": 1.0, "b": 1.0, "c": 1.0}},
			arr{obj{"a": 1.0}, obj{"a": 1.0, "b": 1.0}},
		},
		{
			"properties", `{"properties":{"a":{"type":"string"},"b~/c":{"type":"integer"}}}`,
			arr{obj{}, obj{"a": "x", "b~/c": 1.0, "z": nil}},
			arr{obj{"a": 1.0}, obj{"b~/c": "1"}},
		},
		{
			"patternProperties", `{"patternProperties":{"^x-":{"type":"string"},"y$":{"minLength":2}}}`,
			arr{obj{"x-a": "s", "zy": "ab", "other": 1.0}},
			arr{obj{"x-a": 1.0}, obj{"ay": "a"}, obj{"x-y": "a"}},
		},
		{
			"additionalProperties false past properties and patternProperties",
			`{"properties":{"a":true},"patternProperties":{"^x-":true},"additionalProperties":false}`,
			arr{obj{}, obj{"a": 1.0, "x-b": 2.0}},
			arr{obj{"c": 1.0}, obj{"a": 1.0, "y-b": 2.0}},
		},
		{
			"additionalProperties of a schema", `{"properties":{"a":true},"additionalProperties":{"type":"string"}}`,
			arr{obj{"a": 1.0, "b": "s"}},
			arr{obj{"b": 1.0}},
		},
		{
			"propertyNames", `{"propertyNames":{"maxLength":3,"pattern":"^[a-z]+$"}}`,
			arr{obj{}, obj{"abc": 1.0}},
			arr{obj{"abcd": 1.0}, obj{"A": 1.0}},
		},
		{"propertyNames of an enum", `{"propertyNames":{"enum":["a","b"]}}`, arr{obj{"a": 1.0, "b": 2.0}}, arr{obj{"c": 1.0}}},
		{
			"prefixItems and items", `{"prefixItems":[{"type":"string"},{"type":"integer"}],"items":{"type":"boolean"}}`,
			arr{arr{}, arr{"a"}, arr{"a", 1.0, true, false}},
			arr{arr{1.0}, arr{"a", "b"}, arr{"a", 1.0, "x"}},
		},
		{"items false past prefixItems", `{"prefixItems":[true,true],"items":false}`, arr{arr{}, arr{1.0, 2.0}}, arr{arr{1.0, 2.0, 3.0}}},
		{"items", `{"items":{"type":"integer"}}`, arr{arr{}, arr{1.0, 2.0}}, arr{arr{1.0, "2"}}},
		{"allOf", `{"allOf":[{"type":"integer"},{"minimum":2}]}`, arr{2.0}, arr{1.0, 2.5}},
		{"anyOf", `{"anyOf":[{"type":"string"},{"minimum":2}]}`, arr{"a", 3.0}, arr{1.0}},
		{"oneOf matches exactly one", `{"oneOf":[{"type":"integer"},{"minimum":2}]}`, arr{1.0, 2.5}, arr{3.0, 1.5}},
		{"not", `{"not":{"type":"string"}}`, arr{1.0, nil}, arr{"x"}},
		{"if, then and else", `{"if":{"type":"string"},"then":{"minLength":2},"else":{"minimum":10}}`, arr{"ab", 10.0}, arr{"a", 9.0}},
		{"if and then without else", `{"if":{"type":"string"},"then":{"minLength":2}}`, arr{"ab", 1.0}, arr{"a"}},
		{"then and else without if", `{"then":false,"else":false}`, arr{1.0, "a"}, nil},
		{
			"dependentSchemas", `{"dependentSchemas":{"a":{"required":["b"]}}}`,
			arr{obj{}, obj{"a": 1.0, "b": 2.0}, obj{"b": 2.0}},
			arr{obj{"a": 1.0}},
		},
		{"true", `true`, arr{1.0, nil, obj{}}, nil},
		{"false", `false`, nil, arr{1.0, nil, obj{}}},
		{"the empty schema", `{}`, arr{1.0, "a", nil, arr{}, obj{}}, nil},
		{
			"annotations assert nothing",
			`{"title":"t","description":"d","default":1,"examples":[1],"deprecated":true,"readOnly":true,` +
				`"writeOnly":true,"$comment":"c","format":"email","contentEncoding":"base64",` +
				`"contentMediaType":"application/json","contentSchema":{"type":"object"},"x-kind":{"type":"number"}}`,
			arr{"not an email", "%%%", 1.0},
			nil,
		},
		{
			"unknown keywords assert nothing",
			`{"definitions":{"a":{"type":"string"}},"additionalItems":false,"dependencies":{"a":["b"]},"foo":{"type":"string"}}`,
			arr{1.0, arr{1.0}, obj{"a": 1.0}},
			nil,
		},
	} {
		for _, v := range tc.valid {
			if err := atMember(t, tc.schema, v); err != nil {
				t.Errorf("%s: %s against %#v = %v, want nil", tc.name, tc.schema, v, err)
			}
		}
		for _, v := range tc.invalid {
			var verr *ValidationError
			if err := atMember(t, tc.schema, v); !errors.As(err, &verr) {
				t.Errorf("%s: %s against %#v = %v, want a *ValidationError", tc.name, tc.schema, v, err)
			}
		}
	}
}

// TestRefsResolveInsideTheSchema resolves $ref by each form the subset has:
// the root, a JSON pointer, escaped and percent-encoded, and an anchor.
func TestRefsResolveInsideTheSchema(t *testing.T) {
	for name, tc := range map[string]struct {
		schema  string
		valid   []obj
		invalid []obj
	}{
		"a pointer": {
			`{"$defs":{"port":{"type":"integer"}},"properties":{"p":{"$ref":"#/$defs/port"}}}`,
			[]obj{{"p": 80.0}},
			[]obj{{"p": "80"}},
		},
		"an anchor": {
			`{"$defs":{"port":{"$anchor":"port","type":"integer"}},"properties":{"p":{"$ref":"#port"}}}`,
			[]obj{{"p": 80.0}},
			[]obj{{"p": "80"}},
		},
		"escaped and percent-encoded pointers": {
			`{"$defs":{"a/b":{"type":"integer"},"c~d":{"type":"string"},"e f":{"type":"boolean"}},` +
				`"properties":{"x":{"$ref":"#/$defs/a~1b"},"y":{"$ref":"#/$defs/c~0d"},"z":{"$ref":"#/$defs/e%20f"}}}`,
			[]obj{{"x": 1.0, "y": "s", "z": true}},
			[]obj{{"x": "1"}, {"y": 1.0}, {"z": "true"}},
		},
		"a pointer into an array": {
			`{"allOf":[{"properties":{"a":{"type":"string"}}}],"properties":{"b":{"$ref":"#/allOf/0/properties/a"}}}`,
			[]obj{{"a": "x", "b": "y"}},
			[]obj{{"b": 1.0}},
		},
		"a pointer into a keyword the subset does not know": {
			`{"definitions":{"s":{"type":"string"}},"properties":{"a":{"$ref":"#/definitions/s"}}}`,
			[]obj{{"a": "x"}},
			[]obj{{"a": 1.0}},
		},
		"$ref and the keywords beside it": {
			`{"$defs":{"s":{"type":"string"}},"properties":{"a":{"$ref":"#/$defs/s","maxLength":2}}}`,
			[]obj{{"a": "ab"}},
			[]obj{{"a": "abc"}, {"a": 1.0}},
		},
		"a recursive tree": {
			`{"type":"object","required":["value"],"properties":{"value":{"type":"integer"},` +
				`"children":{"type":"array","items":{"$ref":"#"}}}}`,
			[]obj{{"value": 1.0}, {"value": 1.0, "children": arr{obj{"value": 2.0, "children": arr{}}, obj{"value": 3.0, "children": arr{obj{"value": 4.0}}}}}},
			[]obj{{"value": 1.0, "children": arr{obj{"children": arr{}}}}, {"value": 1.0, "children": arr{obj{"value": 2.0, "children": arr{obj{"value": "4"}}}}}},
		},
		"a linked list through the root": {
			`{"required":["id"],"properties":{"next":{"$ref":"#"}}}`,
			[]obj{{"id": 1.0, "next": obj{"id": 2.0}}},
			[]obj{{"id": 1.0, "next": obj{"next": obj{"id": 3.0}}}},
		},
	} {
		s, err := Compile([]byte(tc.schema))
		if err != nil {
			t.Errorf("%s: Compile: %v", name, err)
			continue
		}
		for _, body := range tc.valid {
			if err := s.Validate(body); err != nil {
				t.Errorf("%s: Validate(%v) = %v, want nil", name, body, err)
			}
		}
		for _, body := range tc.invalid {
			var verr *ValidationError
			if err := s.Validate(body); !errors.As(err, &verr) {
				t.Errorf("%s: Validate(%v) = %v, want a *ValidationError", name, body, err)
			}
		}
	}
}

// TestValidateNamesEachProblem pins what a problem says for each keyword, and
// where.
func TestValidateNamesEachProblem(t *testing.T) {
	for _, tc := range []struct {
		schema string
		body   obj
		want   []Problem
	}{
		{`{"required":["a","b"]}`, obj{}, []Problem{{"", "missing properties 'a', 'b'"}}},
		{`{"properties":{"a":{"type":["string","null"]}}}`, obj{"a": 1.0}, []Problem{{"/a", "got number, want string or null"}}},
		{`{"properties":{"a":{"maximum":1.5}}}`, obj{"a": 2.0}, []Problem{{"/a", "maximum: got 2, want 1.5"}}},
		{`{"properties":{"a":{"exclusiveMinimum":0}}}`, obj{"a": jsonNumber("-1e-30")}, []Problem{{"/a", "exclusiveMinimum: got -1e-30, want 0"}}},
		{`{"properties":{"a":{"multipleOf":0.5}}}`, obj{"a": 0.7}, []Problem{{"/a", "multipleOf: got 0.7, want 0.5"}}},
		{`{"properties":{"a":{"minLength":2}}}`, obj{"a": "x"}, []Problem{{"/a", "minLength: got 1, want 2"}}},
		{`{"properties":{"a":{"pattern":"^x"}}}`, obj{"a": "y"}, []Problem{{"/a", "'y' does not match pattern '^x'"}}},
		{`{"properties":{"a":{"maxItems":1}}}`, obj{"a": arr{1.0, 2.0}}, []Problem{{"/a", "maxItems: got 2, want 1"}}},
		{`{"properties":{"a":{"uniqueItems":true}}}`, obj{"a": arr{1.0, 2.0, 1.0}}, []Problem{{"/a", "items at 0 and 2 are equal"}}},
		{`{"properties":{"a":{"prefixItems":[true],"items":false}}}`, obj{"a": arr{1.0, 2.0}}, []Problem{{"/a", "items: got 2, want at most 1"}}},
		{`{"properties":{"a":{"contains":{"type":"string"}}}}`, obj{"a": arr{1.0}}, []Problem{{"/a", "no items match contains schema"}}},
		{
			`{"properties":{"a":{"contains":{"type":"string"},"minContains":2}}}`,
			obj{"a": arr{"x", 1.0}},
			[]Problem{{"/a", "min 2 items required to match contains schema, but matched 1"}},
		},
		{
			`{"properties":{"a":{"contains":{"type":"string"},"maxContains":1}}}`,
			obj{"a": arr{"x", "y"}},
			[]Problem{{"/a", "max 1 items allowed to match contains schema, but matched more"}},
		},
		{`{"minProperties":1}`, obj{}, []Problem{{"", "minProperties: got 0, want 1"}}},
		{`{"dependentRequired":{"a":["b"]}}`, obj{"a": 1.0}, []Problem{{"", "property 'b' required, if 'a' exists"}}},
		{`{"propertyNames":{"maxLength":1}}`, obj{"ab": 1.0}, []Problem{{"", "invalid propertyName 'ab'"}}},
		{
			`{"additionalProperties":false,"properties":{"a":true}}`,
			obj{"a": 1.0, "b": 1.0, "c": 1.0},
			[]Problem{{"", "additional properties 'b', 'c' not allowed"}},
		},
		{`{"properties":{"a":{"anyOf":[{"type":"string"},{"type":"null"}]}}}`, obj{"a": 1.0}, []Problem{{"/a", "'anyOf' failed, none matched"}}},
		{`{"properties":{"a":{"oneOf":[true,true]}}}`, obj{"a": 1.0}, []Problem{{"/a", "'oneOf' failed, subschemas 0, 1 matched"}}},
		{`{"properties":{"a":{"oneOf":[false,false]}}}`, obj{"a": 1.0}, []Problem{{"/a", "'oneOf' failed, none matched"}}},
		{`{"properties":{"a":{"not":true}}}`, obj{"a": 1.0}, []Problem{{"/a", "'not' failed"}}},
		{`{"properties":{"a":false}}`, obj{"a": 1.0}, []Problem{{"/a", "false schema"}}},
		{`{"properties":{"a":{"const":"x"}}}`, obj{"a": "y"}, []Problem{{"/a", "value must be 'x'"}}},
		{`{"properties":{"a":{"enum":[1,2.5,null]}}}`, obj{"a": "y"}, []Problem{{"/a", "value must be one of 1, 2.5, null"}}},
		{`{"properties":{"a":{"enum":[{"b":1}]}}}`, obj{"a": "y"}, []Problem{{"/a", "'enum' failed"}}},
		{
			`{"properties":{"a":{"if":{"type":"string"},"then":{"minLength":3}}}}`,
			obj{"a": "y"},
			[]Problem{{"/a", "minLength: got 1, want 3"}},
		},
		{
			// A problem found twice, through two subschemas, is one problem.
			`{"allOf":[{"required":["a"]},{"required":["a"]}]}`,
			obj{},
			[]Problem{{"", "missing property 'a'"}},
		},
	} {
		s, err := Compile([]byte(tc.schema))
		if err != nil {
			t.Errorf("Compile(%s): %v", tc.schema, err)
			continue
		}
		if got := problems(t, s, tc.body); !slices.Equal(got, tc.want) {
			t.Errorf("%s against %v: problems %q, want %q", tc.schema, tc.body, got, tc.want)
		}
	}
}
