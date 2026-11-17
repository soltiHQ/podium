package workloadschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A workload schema is draft 2020-12, in a subset whose evaluation the
// evaluator bounds (eval.go). Compile refuses any other draft, and each
// keyword of draft 2020-12 outside the subset, by its name and location:
//
//   - $schema: none, or draft 2020-12's URI exactly.
//   - $id at the root only; $anchor, a plain name; $defs.
//   - $ref only inside the document: "#", "#/json/pointer" (RFC 6901,
//     percent-encoded as a URI fragment is) or "#anchor". The keywords beside
//     a $ref apply as well.
//   - Applicators: allOf, anyOf, oneOf, not, if, then, else, dependentSchemas,
//     properties, patternProperties, additionalProperties, propertyNames,
//     prefixItems, items, contains.
//   - Assertions: type, enum, const, multipleOf, maximum, exclusiveMaximum,
//     minimum, exclusiveMinimum, maxLength, minLength, pattern, maxItems,
//     minItems, uniqueItems, maxContains, minContains, maxProperties,
//     minProperties, required, dependentRequired.
//   - Annotations, which assert nothing: title, description, default,
//     deprecated, readOnly, writeOnly, examples, $comment, format,
//     contentEncoding, contentMediaType, contentSchema, and any keyword that
//     begins with "x-".
//
// Any other keyword is ignored, as draft 2020-12 ignores keywords it does not
// know, and its value is not compiled unless a $ref points into it. A keyword
// of the subset whose value is not what the keyword takes is an error. A
// pattern, and a patternProperties key, is Go's regular expression syntax
// without inline flag groups, within the bounds that regex.go sets.

// draft2020 is the one $schema a workload schema may have.
const draft2020 = "https://json-schema.org/draft/2020-12/schema"

// maxCount is the most a counting keyword, such as maxLength, takes: a count
// over it is clamped to it, which no body within its bounds comes near.
const maxCount = math.MaxInt32

// refusedKeywords are the keywords of draft 2020-12, and of 2019-09, that the
// subset leaves out, and why.
var refusedKeywords = map[string]string{
	"$dynamicRef":           "it resolves at run time",
	"$dynamicAnchor":        "it resolves at run time",
	"$recursiveRef":         "it resolves at run time",
	"$recursiveAnchor":      "it resolves at run time",
	"$vocabulary":           "it belongs to metaschemas",
	"unevaluatedItems":      "it depends on what the keywords beside it evaluated",
	"unevaluatedProperties": "it depends on what the keywords beside it evaluated",
}

// anchorName is the form of a plain-name anchor.
var anchorName = regexp.MustCompile(`^[A-Za-z_][-A-Za-z0-9._]*$`)

// The types a schema accepts, as a mask.
const (
	typeNull uint8 = 1 << iota
	typeBoolean
	typeNumber
	typeString
	typeArray
	typeObject
	typeInteger
)

var typeBits = map[string]uint8{
	"null": typeNull, "boolean": typeBoolean, "number": typeNumber, "string": typeString,
	"array": typeArray, "object": typeObject, "integer": typeInteger,
}

// kindBits is the type of each kind of value. A number with no fractional
// part is an integer too.
var kindBits = [...]uint8{
	nullKind: typeNull, booleanKind: typeBoolean, numberKind: typeNumber,
	stringKind: typeString, arrayKind: typeArray, objectKind: typeObject,
}

// schema is a compiled subschema.
type schema struct {
	// at is where it is in the document, for the errors of Compile, which
	// clears it once it is done.
	at *location

	boolean bool // a schema of true or false, which accepts every value or none
	valid   bool // the value of a boolean schema

	keywords int   // the keywords it evaluates
	cost     int64 // the steps of one evaluation of it, besides its loops and leaves

	// Assertions.
	types     uint8  // the types it accepts, 0 for any
	typesText string // their names, for problems: "string or null"
	enum      []*value
	enumIndex map[uint64][]int // the positions of the values of a long enum, by hash
	enumText  string
	constant  *value
	constText string

	multipleOf       *multiple
	maximum          *num
	exclusiveMaximum *num
	minimum          *num
	exclusiveMinimum *num

	minLength, maxLength int // -1 when absent
	pattern              *pattern

	minItems, maxItems       int // -1 when absent
	uniqueItems              bool
	minContains, maxContains int // 1 and -1 when absent

	minProperties, maxProperties int // -1 when absent
	required                     []string
	dependentRequired            []dependentNames

	// Applicators at the value the schema validates.
	ref                    *schema
	allOf, anyOf, oneOf    []*schema
	not                    *schema
	ifSchema               *schema
	thenSchema, elseSchema *schema
	dependentSchemas       []namedSchema

	// Applicators at the members of an object and the items of an array.
	properties           map[string]*schema
	propertyList         []namedSchema // properties, sorted by name
	patternProperties    []patternSchema
	additionalProperties *schema
	propertyNames        *schema
	prefixItems          []*schema
	items                *schema
	contains             *schema

	// What the evaluator reads before the keywords: whether any of them
	// applies to a value of a type, and which special forms they take.
	readsNumber, readsString, readsArray, readsObject bool
	appliesToItems, appliesToMembers                  bool
	itemsFalse, additionalFalse                       bool
}

type namedSchema struct {
	name   string
	schema *schema
}

type patternSchema struct {
	pattern *pattern
	schema  *schema
}

type dependentNames struct {
	name  string
	names []string
}

// location is where a value is in the document: the location of the value
// that holds it, and its RFC 6901 token there. Compile writes it out only for
// an error, cut to maxMessageBytes, so that no subschema keeps the names of
// its ancestors.
type location struct {
	parent *location
	token  string
}

// child is the location tokens below l.
func (l *location) child(tokens ...string) *location {
	for _, t := range tokens {
		l = &location{parent: l, token: t}
	}
	return l
}

// maxLocationBytes bounds a location in an error, so that the keyword before
// it and the reason after it fit in maxMessageBytes.
const maxLocationBytes = 256

// String writes the location as a URI fragment, "#/properties/name", cut to
// maxLocationBytes at a rune boundary. The nil location is the root, "#".
func (l *location) String() string {
	var tokens []string
	for at := l; at != nil; at = at.parent {
		tokens = append(tokens, at.token)
	}
	var b strings.Builder
	b.WriteByte('#')
	for i := len(tokens) - 1; i >= 0 && b.Len() <= maxLocationBytes; i-- {
		t := tokens[i]
		if len(t) > maxLocationBytes {
			t = t[:maxLocationBytes+1] // past the cut, which cut makes at a rune boundary
		}
		b.WriteByte('/')
		b.WriteString(escapeToken(t))
	}
	return cut(b.String(), maxLocationBytes, "…")
}

// quoted is the location quoted for an error, cut to maxLocationBytes once
// quoted.
func (l *location) quoted() string {
	return cut(strconv.Quote(l.String()), maxLocationBytes, `…"`)
}

// position is the place of a value in the decoded document, by which a
// subschema compiled there is found again: the object or the array that
// holds it, by address, and its member name or index there. The root's is
// the zero position.
type position struct {
	in  uintptr
	key string
}

// positionIn is the position of the value under key in the object or the
// array container.
func positionIn(container any, key string) position {
	return position{in: reflect.ValueOf(container).Pointer(), key: key}
}

// compiler compiles the subschemas of one schema document.
type compiler struct {
	doc      any
	nodes    map[position]*schema // the subschemas compiled, by position
	order    []*schema            // the same, in the order compiled
	anchors  map[string]*schema
	refs     []pendingRef // the $refs to resolve once the tree is compiled
	patterns regexBounds  // what the regular expressions have taken of their bounds
}

type pendingRef struct {
	from *schema
	ref  string
}

// compileDocument compiles a decoded schema document.
//
// It compiles the tree of subschemas first, from the root through the
// applicators and $defs, which registers each $anchor. Then it resolves each
// $ref, compiling a target that is not in the tree, such as one under a
// keyword the subset does not know. Last, it refuses a cycle of subschemas at
// one location of a body.
func compileDocument(doc any) (*schema, error) {
	c := &compiler{doc: doc, nodes: map[position]*schema{}, anchors: map[string]*schema{}}
	root, err := c.compile(doc, nil, position{}, true)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(c.refs); i++ { // a target compiled here may add more
		r := c.refs[i]
		target, err := c.resolve(r.from, r.ref)
		if err != nil {
			return nil, err
		}
		r.from.ref = target
	}
	if err := c.checkCycles(); err != nil {
		return nil, err
	}
	for _, s := range c.order {
		s.finish()
	}
	return root, nil
}

// compile compiles the subschema v, at a location and a position. A
// subschema of the tree registers its $anchor.
func (c *compiler) compile(v any, at *location, pos position, tree bool) (*schema, error) {
	if s, ok := c.nodes[pos]; ok {
		return s, nil
	}
	s := &schema{
		at: at, minLength: -1, maxLength: -1, minItems: -1, maxItems: -1,
		minContains: 1, maxContains: -1, minProperties: -1, maxProperties: -1,
	}
	c.nodes[pos] = s
	c.order = append(c.order, s)
	switch v := v.(type) {
	case bool:
		s.boolean, s.valid = true, v
		return s, nil
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if err := c.keyword(s, v, k, tree); err != nil {
				return nil, err
			}
		}
		return s, nil
	}
	return nil, fmt.Errorf("workloadschema: compile schema: the value at %s is %s, not a schema: want an object or a boolean", at.quoted(), describe(v))
}

// keyword compiles the keyword k of the subschema s, whose object is obj.
func (c *compiler) keyword(s *schema, obj map[string]any, k string, tree bool) error {
	v := obj[k]
	fail := func(format string, args ...any) error {
		return fmt.Errorf("workloadschema: compile schema: %q at %s "+format, append([]any{k, s.at.quoted()}, args...)...)
	}
	// sub compiles the subschema under the keyword itself, or under one of
	// its members or items, key, in container.
	sub := func(v any) (*schema, error) {
		return c.compile(v, s.at.child(k), positionIn(obj, k), tree)
	}
	subAt := func(v, container any, key string) (*schema, error) {
		return c.compile(v, s.at.child(k, key), positionIn(container, key), tree)
	}
	switch k {
	case "$schema":
		if text, ok := v.(string); !ok || text != draft2020 {
			return fail("is %s: a workload schema is draft 2020-12, %q, or has no $schema", describe(v), draft2020)
		}
		return nil
	case "$id":
		if s.at != nil {
			return fail("is not supported: only the root of a workload schema may have an $id")
		}
		if _, ok := v.(string); !ok {
			return fail("must be a string")
		}
		return nil
	case "$anchor":
		name, ok := v.(string)
		if !ok || !anchorName.MatchString(name) {
			return fail("must be a plain name: a letter or '_', then letters, digits, '-', '_' and '.'")
		}
		if tree {
			if other, ok := c.anchors[name]; ok {
				return fail("declares %s, which %s declares too", quote(name), other.at.quoted())
			}
			c.anchors[name] = s
		}
		return nil
	case "$ref":
		ref, ok := v.(string)
		if !ok {
			return fail("must be a string")
		}
		if !strings.HasPrefix(ref, "#") {
			return fail(`is a remote reference, %s: a workload schema refers only inside itself, by "#", "#/json/pointer" or "#anchor"`, quote(ref))
		}
		c.refs = append(c.refs, pendingRef{from: s, ref: ref})
	case "$defs":
		defs, ok := v.(map[string]any)
		if !ok {
			return fail("must be an object of schemas")
		}
		for _, name := range slices.Sorted(maps.Keys(defs)) {
			if _, err := subAt(defs[name], defs, name); err != nil {
				return err
			}
		}
		return nil
	case "$comment", "title", "description", "format", "contentEncoding", "contentMediaType":
		if _, ok := v.(string); !ok {
			return fail("must be a string")
		}
		return nil
	case "deprecated", "readOnly", "writeOnly":
		if _, ok := v.(bool); !ok {
			return fail("must be a boolean")
		}
		return nil
	case "examples":
		if _, ok := v.([]any); !ok {
			return fail("must be an array")
		}
		return nil
	case "default":
		return nil
	case "contentSchema":
		if !isSchema(v) {
			return fail("must be a schema: an object or a boolean")
		}
		return nil
	case "type":
		if err := s.compileTypes(v); err != nil {
			return fail("%w", err)
		}
	case "enum":
		list, ok := v.([]any)
		if !ok {
			return fail("must be an array")
		}
		s.enum = make([]*value, 0, len(list))
		for _, x := range list {
			e, err := constant(x)
			if err != nil {
				return fail("holds %v", err)
			}
			s.enum = append(s.enum, e)
		}
		s.enumText = enumText(s.enum)
		if len(s.enum) > 8 {
			s.enumIndex = make(map[uint64][]int, len(s.enum))
			for i, e := range s.enum {
				s.enumIndex[e.hash] = append(s.enumIndex[e.hash], i)
			}
		}
	case "const":
		e, err := constant(v)
		if err != nil {
			return fail("holds %v", err)
		}
		s.constant, s.constText = e, constText(e)
	case "multipleOf":
		n, err := number(v)
		if err != nil || n.sign() <= 0 {
			return fail("must be a number over zero")
		}
		s.multipleOf = newMultiple(n)
	case "maximum", "exclusiveMaximum", "minimum", "exclusiveMinimum":
		n, err := number(v)
		if err != nil {
			return fail("must be a number")
		}
		switch k {
		case "maximum":
			s.maximum = &n
		case "exclusiveMaximum":
			s.exclusiveMaximum = &n
		case "minimum":
			s.minimum = &n
		default:
			s.exclusiveMinimum = &n
		}
	case "maxLength", "minLength", "maxItems", "minItems", "maxContains", "minContains", "maxProperties", "minProperties":
		n, err := count(v)
		if err != nil {
			return fail("must be a non-negative integer")
		}
		switch k {
		case "maxLength":
			s.maxLength = n
		case "minLength":
			s.minLength = n
		case "maxItems":
			s.maxItems = n
		case "minItems":
			s.minItems = n
		case "maxContains":
			s.maxContains = n
		case "minContains":
			s.minContains = n
		case "maxProperties":
			s.maxProperties = n
		default:
			s.minProperties = n
		}
	case "pattern":
		text, ok := v.(string)
		if !ok {
			return fail("must be a string")
		}
		p, err := c.patterns.compile(text)
		if err != nil {
			return fail("%w", err)
		}
		s.pattern = p
	case "uniqueItems":
		unique, ok := v.(bool)
		if !ok {
			return fail("must be a boolean")
		}
		s.uniqueItems = unique
	case "required":
		names, err := uniqueNames(v)
		if err != nil {
			return fail("%w", err)
		}
		s.required = names
	case "dependentRequired":
		deps, ok := v.(map[string]any)
		if !ok {
			return fail("must be an object of arrays of names")
		}
		for _, name := range slices.Sorted(maps.Keys(deps)) {
			names, err := uniqueNames(deps[name])
			if err != nil {
				return fail("at %s %w", quote(name), err)
			}
			s.dependentRequired = append(s.dependentRequired, dependentNames{name: name, names: names})
		}
	case "allOf", "anyOf", "oneOf", "prefixItems":
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			return fail("must be a non-empty array of schemas")
		}
		schemas := make([]*schema, len(list))
		for i, x := range list {
			var err error
			if schemas[i], err = subAt(x, list, strconv.Itoa(i)); err != nil {
				return err
			}
		}
		switch k {
		case "allOf":
			s.allOf = schemas
		case "anyOf":
			s.anyOf = schemas
		case "oneOf":
			s.oneOf = schemas
		default:
			s.prefixItems = schemas
		}
	case "not", "if", "then", "else", "additionalProperties", "propertyNames", "items", "contains":
		if !isSchema(v) {
			if k == "items" {
				return fail("must be a schema: an object or a boolean (in draft 2020-12, prefixItems holds the schemas of the first items)")
			}
			return fail("must be a schema: an object or a boolean")
		}
		one, err := sub(v)
		if err != nil {
			return err
		}
		switch k {
		case "not":
			s.not = one
		case "if":
			s.ifSchema = one
		case "then":
			s.thenSchema = one
		case "else":
			s.elseSchema = one
		case "additionalProperties":
			s.additionalProperties = one
		case "propertyNames":
			s.propertyNames = one
		case "items":
			s.items = one
		default:
			s.contains = one
		}
	case "properties", "dependentSchemas":
		m, ok := v.(map[string]any)
		if !ok {
			return fail("must be an object of schemas")
		}
		list := make([]namedSchema, 0, len(m))
		for _, name := range slices.Sorted(maps.Keys(m)) {
			one, err := subAt(m[name], m, name)
			if err != nil {
				return err
			}
			list = append(list, namedSchema{name: name, schema: one})
		}
		if k == "dependentSchemas" {
			s.dependentSchemas = list
			break
		}
		s.propertyList = list
		s.properties = make(map[string]*schema, len(list))
		for _, p := range list {
			s.properties[p.name] = p.schema
		}
	case "patternProperties":
		m, ok := v.(map[string]any)
		if !ok {
			return fail("must be an object of schemas")
		}
		for _, text := range slices.Sorted(maps.Keys(m)) {
			p, err := c.patterns.compile(text)
			if err != nil {
				return fail("%s %w", quote(text), err)
			}
			one, err := subAt(m[text], m, text)
			if err != nil {
				return err
			}
			s.patternProperties = append(s.patternProperties, patternSchema{pattern: p, schema: one})
		}
	default:
		if why, ok := refusedKeywords[k]; ok {
			return fail("is not supported: %s", why)
		}
		// A keyword that begins with "x-", or that the subset does not know.
		return nil
	}
	s.keywords++
	return nil
}

// compileTypes compiles the value of the type keyword.
func (s *schema) compileTypes(v any) error {
	var names []string
	switch v := v.(type) {
	case string:
		names = []string{v}
	case []any:
		for _, x := range v {
			name, ok := x.(string)
			if !ok {
				return errors.New("must be a type's name, or a non-empty array of them")
			}
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return errors.New("must be a type's name, or a non-empty array of them")
	}
	for _, name := range names {
		bit, ok := typeBits[name]
		if !ok {
			return fmt.Errorf("names %s, which is not a type: want null, boolean, object, array, number, string or integer", quote(name))
		}
		if s.types&bit != 0 {
			return fmt.Errorf("names %s twice", quote(name))
		}
		s.types |= bit
	}
	s.typesText = strings.Join(names, " or ")
	return nil
}

// resolve finds the subschema that a $ref inside the document points at.
func (c *compiler) resolve(from *schema, ref string) (*schema, error) {
	fail := func(format string, args ...any) error {
		return fmt.Errorf(`workloadschema: compile schema: "$ref" at %s is %s, `+format, append([]any{from.at.quoted(), quote(ref)}, args...)...)
	}
	frag := ref[1:]
	if frag == "" {
		return c.nodes[position{}], nil
	}
	if frag[0] != '/' {
		if !anchorName.MatchString(frag) {
			return nil, fail("neither a JSON pointer nor a plain-name anchor")
		}
		target, ok := c.anchors[frag]
		if !ok {
			return nil, fail("but no subschema declares the $anchor %s", quote(frag))
		}
		return target, nil
	}
	decoded, err := url.PathUnescape(frag)
	if err != nil {
		return nil, fail("not a valid URI fragment: %v", err)
	}
	tokens := strings.Split(decoded[1:], "/")
	for i, t := range tokens {
		if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(t, "~0", ""), "~1", ""), "~") {
			return nil, fail("whose JSON pointer has a '~' that is neither ~0 nor ~1")
		}
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	v, pos := c.doc, position{}
	for _, t := range tokens {
		switch node := v.(type) {
		case map[string]any:
			child, ok := node[t]
			if !ok {
				return nil, fail("which points at nothing in the schema")
			}
			v, pos = child, positionIn(node, t)
		case []any:
			i, ok := arrayIndex(t, len(node))
			if !ok {
				return nil, fail("which points at nothing in the schema")
			}
			v, pos = node[i], positionIn(node, t)
		default:
			return nil, fail("which points at nothing in the schema")
		}
	}
	if !isSchema(v) {
		return nil, fail("which points at %s, not a schema", describe(v))
	}
	return c.compile(v, (*location)(nil).child(tokens...), pos, false)
}

// arrayIndex reads an RFC 6901 array index below n.
func arrayIndex(t string, n int) (int, bool) {
	if t == "" || len(t) > 1 && t[0] == '0' {
		return 0, false
	}
	i, err := strconv.Atoi(t)
	if err != nil || i < 0 || i >= n {
		return 0, false
	}
	return i, true
}

// checkCycles refuses a cycle of subschemas through the applicators that
// evaluate a subschema at the value they validate: one would evaluate without
// end on every body.
func (c *compiler) checkCycles() error {
	const visiting, done = 1, 2
	state := make(map[*schema]int, len(c.order))
	var visit func(*schema) error
	visit = func(s *schema) error {
		switch state[s] {
		case visiting:
			return fmt.Errorf("workloadschema: compile schema: the subschema at %s refers back to itself at the same location of a body (a reference cycle)", s.at.quoted())
		case done:
			return nil
		}
		state[s] = visiting
		for _, next := range s.sameLocation() {
			if err := visit(next); err != nil {
				return err
			}
		}
		state[s] = done
		return nil
	}
	for _, s := range c.order {
		if err := visit(s); err != nil {
			return err
		}
	}
	return nil
}

// sameLocation is the subschemas that s evaluates at the value it validates.
// Then and else apply only beside an if.
func (s *schema) sameLocation() []*schema {
	var out []*schema
	for _, one := range []*schema{s.ref, s.not, s.ifSchema} {
		if one != nil {
			out = append(out, one)
		}
	}
	if s.ifSchema != nil {
		for _, one := range []*schema{s.thenSchema, s.elseSchema} {
			if one != nil {
				out = append(out, one)
			}
		}
	}
	out = append(out, s.allOf...)
	out = append(out, s.anyOf...)
	out = append(out, s.oneOf...)
	for _, d := range s.dependentSchemas {
		out = append(out, d.schema)
	}
	return out
}

// finish sets what the evaluator reads of a subschema before its keywords:
// its cost, and which of its keywords apply to a value of which type.
func (s *schema) finish() {
	s.at = nil // for errors of Compile only
	s.cost = stepsPerEval + int64(s.keywords)*stepsPerKeyword
	if s.boolean {
		return
	}
	s.readsNumber = s.multipleOf != nil || s.maximum != nil || s.exclusiveMaximum != nil || s.minimum != nil || s.exclusiveMinimum != nil
	s.readsString = s.minLength >= 0 || s.maxLength >= 0 || s.pattern != nil
	s.readsArray = s.minItems >= 0 || s.maxItems >= 0 || s.uniqueItems
	s.readsObject = s.minProperties >= 0 || s.maxProperties >= 0 || s.required != nil || s.dependentRequired != nil
	s.appliesToItems = s.prefixItems != nil || s.items != nil || s.contains != nil
	s.appliesToMembers = s.dependentSchemas != nil || s.properties != nil || s.patternProperties != nil ||
		s.additionalProperties != nil || s.propertyNames != nil
	s.itemsFalse = s.items != nil && s.items.boolean && !s.items.valid
	s.additionalFalse = s.additionalProperties != nil && s.additionalProperties.boolean && !s.additionalProperties.valid
}

// isSchema reports a value that may be a schema: an object or a boolean.
func isSchema(v any) bool {
	switch v.(type) {
	case map[string]any, bool:
		return true
	}
	return false
}

// constant makes the value of an enum or a const.
func constant(x any) (*value, error) {
	b := &builder{}
	v := b.build(x)
	if b.bad != nil {
		// Not reached: canonical JSON writes each number as a double.
		return nil, errors.New(b.bad.Problems[0].Message)
	}
	return v, nil
}

// number reads a number of a schema.
func number(v any) (num, error) {
	n, ok := v.(json.Number)
	if !ok {
		return num{}, errNumberSyntax
	}
	return parseNum(string(n))
}

// count reads a non-negative integer of a schema, clamped to maxCount.
func count(v any) (int, error) {
	n, err := number(v)
	if err != nil || n.neg || !n.isInteger() {
		return 0, errNumberSyntax
	}
	if n.d == "" {
		return 0, nil
	}
	if len(n.d)+n.e > 10 {
		return maxCount, nil
	}
	x, err := strconv.ParseInt(n.d+strings.Repeat("0", n.e), 10, 64)
	if err != nil || x > maxCount {
		return maxCount, nil
	}
	return int(x), nil
}

// uniqueNames reads an array of distinct names.
func uniqueNames(v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("must be an array of names")
	}
	names := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, x := range list {
		name, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("must be an array of names, and holds %s", describe(x))
		}
		if seen[name] {
			return nil, fmt.Errorf("names %s twice", quote(name))
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}

// escapeToken escapes an RFC 6901 token: ~ as ~0, and / as ~1.
func escapeToken(t string) string {
	return strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1")
}

// describe names a value of a schema for an error: a string as itself, and
// any other value by its type.
func describe(v any) string {
	switch v := v.(type) {
	case string:
		return quote(v)
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return "a number"
}

// enumText is the problem of a value that is not in an enum.
func enumText(values []*value) string {
	if len(values) == 0 {
		return "'enum' failed: it allows no value"
	}
	var b strings.Builder
	for i, v := range values {
		if v.kind == arrayKind || v.kind == objectKind {
			return "'enum' failed"
		}
		if b.Len() > maxMessageBytes {
			continue // the rest is cut, but a later array or object still changes the text
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(display(v))
	}
	if len(values) == 1 {
		return truncate("value must be " + b.String())
	}
	return truncate("value must be one of " + b.String())
}

// constText is the problem of a value that is not the const.
func constText(v *value) string {
	if v.kind == arrayKind || v.kind == objectKind {
		return "'const' failed"
	}
	return truncate("value must be " + display(v))
}

// display writes a scalar value for a problem.
func display(v *value) string {
	switch v.kind {
	case stringKind:
		return quote(v.s)
	case numberKind:
		return v.n.String()
	case booleanKind:
		return strconv.FormatBool(v.b)
	}
	return "null"
}
