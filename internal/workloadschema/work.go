package workloadschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp/syntax"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The validator's work on a body is bounded twice, besides the bounds on the
// body itself.
//
// Compile bounds the work at one location of a body: following $ref and the
// keywords that apply a subschema to the value they sit at, a schema
// evaluates at most maxFanOut subschemas there, and none twice on one path.
//
// That does not bound the whole. A schema that applies two subschemas to the
// same member of an object, and each of them the schema again, doubles its
// work at every level of the body: two anyOf branches into one member take
// 4 s and 1 GB on a body of 19 values nested 18 deep. A schema as plain as
// properties.a → # with a required member the body lacks makes the validator
// copy the path at each level of a deep body: 11 s and 2.8 GB on a body of
// 9 999 values nested 9 998 deep. So Validate walks the body along the schema
// before the validator runs, counts what the validator may do, and refuses a
// body past maxWork.

// maxFanOut bounds the subschemas a schema evaluates at one location of a body.
const maxFanOut = 1000

// The body bounds, which Validate checks before anything else.
const (
	// maxBodyBytes bounds a body as encoded JSON.
	maxBodyBytes = 256 << 10
	// maxBodyValues bounds the JSON values of a body: objects, arrays and
	// scalars alike.
	maxBodyValues = 10_000
)

// The work the validator may do on a body, in steps of about 20 ns, or 140 ns
// under the race detector, measured on each kind of work at the bound:
//
//   - each evaluation of a subschema takes evalSteps, plus depthSteps for each
//     level of its location, which a failure there copies;
//   - at an object, it takes memberSteps for each member, which it loops over;
//   - each number it parses with math/big, to compare it with a value of an
//     enum or a const or to check a numeric keyword, takes compareSteps; a
//     string, a boolean or a null compared takes two steps;
//   - at a string with a string keyword, it takes a step for each byte, and
//     two for each byte and each instruction of each regular expression it
//     matches the string with;
//   - with uniqueItems, it takes memberSteps for each item.
//
// maxWork is 50 000 evaluations. At the bound, each kind of work above took
// at most 0.54 s under the race detector and 0.16 s without it.
const (
	evalSteps    = 64
	depthSteps   = 4
	memberSteps  = 8
	compareSteps = 128
	maxWork      = 50_000 * evalSteps
)

// JSON types, as a mask of those a subschema accepts.
const (
	nullType = 1 << iota
	booleanType
	numberType
	integerType
	stringType
	arrayType
	objectType
)

// plan is a compiled subschema as the validator walks it: what it evaluates at
// the location of the value it validates, and what it applies to the members
// or items of that value.
type plan struct {
	location string // the subschema's place in the schema, for errors
	boolean  bool   // a schema of true or false: one evaluation and no more
	types    int    // the types it accepts, 0 for any; past a mismatch, the validator does nothing more
	compare  int    // the steps of its enum and const comparisons, and of its required members
	numeric  bool   // a numeric keyword, which parses a number of the body
	strings  bool   // a string keyword, which reads a string of the body
	regex    int    // the instructions of its pattern, matched with each string
	unique   bool   // uniqueItems

	// At the same location.
	same      []*plan            // $ref, $dynamicRef, $recursiveRef, allOf, anyOf, oneOf, not, if, then, else
	dependent map[string][]*plan // dependentSchemas and schema dependencies, when the member is there

	// At the members of an object.
	properties map[string]*plan
	patterns   []*plan // patternProperties, assumed to match every member
	nameRegex  int     // the instructions of the patternProperties, matched with each member's name
	additional *plan   // additionalProperties, for members not in properties
	unevalProp *plan   // unevaluatedProperties, assumed to apply to every member
	names      *plan   // propertyNames, which validates each member's name

	// At the items of an array.
	prefix     []*plan // prefixItems, or the items array of an older draft
	items      *plan   // the items past the prefix
	contains   *plan
	unevalItem *plan // unevaluatedItems, assumed to apply to every item

	// The fan-out walk.
	state  int // 0 before the walk, 1 during, 2 after
	fanOut int // the subschemas it evaluates at its location, itself included
}

// newPlan builds the plan of a compiled schema and checks it: at most
// maxFanOut subschemas at any one location of a body, no cycle there, and
// no $dynamicRef that may resolve to more than one subschema. doc is the
// schema as decoded.
func newPlan(compiled *jsonschema.Schema, doc any) (*plan, error) {
	if err := checkDynamicAnchors(doc); err != nil {
		return nil, err
	}
	plans := map[*jsonschema.Schema]*plan{}
	root, err := build(compiled, plans)
	if err != nil {
		return nil, err
	}
	for _, p := range plans {
		if _, err := p.countFanOut(); err != nil {
			return nil, err
		}
	}
	return root, nil
}

// build makes the plan of s and of all it reaches, once each.
func build(s *jsonschema.Schema, plans map[*jsonschema.Schema]*plan) (*plan, error) {
	if p, ok := plans[s]; ok {
		return p, nil
	}
	p := &plan{location: s.Location}
	plans[s] = p
	if s.Bool != nil {
		p.boolean = true
		return p, nil
	}
	var err error
	sub := func(c *jsonschema.Schema) *plan {
		if c == nil || err != nil {
			return nil
		}
		var cp *plan
		cp, err = build(c, plans)
		return cp
	}
	same := func(c *jsonschema.Schema) {
		if cp := sub(c); cp != nil {
			p.same = append(p.same, cp)
		}
	}

	if s.Types != nil {
		for _, t := range s.Types.ToStrings() {
			p.types |= typeMask(t)
		}
	}
	if s.Enum != nil {
		for _, v := range s.Enum.Values {
			p.compare += compareCost(v)
		}
	}
	if s.Const != nil {
		p.compare += compareCost(*s.Const)
	}
	p.compare += len(s.Required)
	for _, names := range s.DependentRequired {
		p.compare += len(names)
	}
	p.numeric = s.Minimum != nil || s.Maximum != nil || s.ExclusiveMinimum != nil ||
		s.ExclusiveMaximum != nil || s.MultipleOf != nil || p.types&integerType != 0
	p.strings = s.MinLength != nil || s.MaxLength != nil || s.Pattern != nil || s.Format != nil
	if s.Pattern != nil {
		p.regex = regexInsts(s.Pattern.String())
	}
	p.unique = s.UniqueItems

	same(s.Ref)
	if s.RecursiveRef != nil {
		if s.RecursiveRef.RecursiveAnchor {
			return nil, fmt.Errorf("workloadschema: the $recursiveRef at %s resolves at run time, which is not supported", s.Location)
		}
		same(s.RecursiveRef)
	}
	if s.DynamicRef != nil {
		// checkDynamicAnchors leaves each anchor one subschema: the one this
		// reference was compiled to.
		same(s.DynamicRef.Ref)
	}
	for _, c := range s.AllOf {
		same(c)
	}
	for _, c := range s.AnyOf {
		same(c)
	}
	for _, c := range s.OneOf {
		same(c)
	}
	same(s.Not)
	same(s.If)
	same(s.Then)
	same(s.Else)
	for name, c := range s.DependentSchemas {
		p.addDependent(name, sub(c))
	}
	for name, d := range s.Dependencies {
		if c, ok := d.(*jsonschema.Schema); ok {
			p.addDependent(name, sub(c))
		}
	}

	for name, c := range s.Properties {
		if p.properties == nil {
			p.properties = map[string]*plan{}
		}
		p.properties[name] = sub(c)
	}
	for re, c := range s.PatternProperties {
		p.patterns = append(p.patterns, sub(c))
		p.nameRegex += regexInsts(re.String())
	}
	if c, ok := s.AdditionalProperties.(*jsonschema.Schema); ok {
		p.additional = sub(c)
	}
	p.unevalProp = sub(s.UnevaluatedProperties)
	p.names = sub(s.PropertyNames)

	switch items := s.Items.(type) { // drafts before 2020-12
	case *jsonschema.Schema:
		p.items = sub(items)
	case []*jsonschema.Schema:
		for _, c := range items {
			p.prefix = append(p.prefix, sub(c))
		}
		if c, ok := s.AdditionalItems.(*jsonschema.Schema); ok {
			p.items = sub(c)
		}
	}
	for _, c := range s.PrefixItems {
		p.prefix = append(p.prefix, sub(c))
	}
	if s.Items2020 != nil {
		p.items = sub(s.Items2020)
	}
	p.contains = sub(s.Contains)
	p.unevalItem = sub(s.UnevaluatedItems)
	return p, err
}

func (p *plan) addDependent(name string, c *plan) {
	if c == nil {
		return
	}
	if p.dependent == nil {
		p.dependent = map[string][]*plan{}
	}
	p.dependent[name] = append(p.dependent[name], c)
}

// countFanOut is the subschemas p evaluates at one location, itself included,
// counted once per path that reaches them, and at most maxFanOut+1.
func (p *plan) countFanOut() (int, error) {
	switch p.state {
	case 1:
		return 0, fmt.Errorf("workloadschema: the subschema at %s refers back to itself at the same location of a body (a reference cycle)", p.location)
	case 2:
		return p.fanOut, nil
	}
	p.state = 1
	n := 1
	add := func(c *plan) error {
		k, err := c.countFanOut()
		n = min(n+k, maxFanOut+1)
		return err
	}
	for _, c := range p.same {
		if err := add(c); err != nil {
			return 0, err
		}
	}
	for _, deps := range p.dependent {
		for _, c := range deps {
			if err := add(c); err != nil {
				return 0, err
			}
		}
	}
	p.state, p.fanOut = 2, n
	if n > maxFanOut {
		return 0, fmt.Errorf("workloadschema: the subschema at %s evaluates more than %d subschemas at one location of a body", p.location, maxFanOut)
	}
	return n, nil
}

// checkDynamicAnchors refuses a schema with a $dynamicRef in which a
// $dynamicAnchor is declared twice: the reference may then resolve to either
// at run time, and the plan follows only the one it was compiled to.
func checkDynamicAnchors(doc any) error {
	declared := map[string]int{}
	dynamicRef := false
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if a, ok := v["$dynamicAnchor"].(string); ok {
				declared[a]++
			}
			if _, ok := v["$dynamicRef"]; ok {
				dynamicRef = true
			}
			for _, c := range v {
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(doc)
	if !dynamicRef {
		return nil
	}
	for a, n := range declared {
		if n > 1 {
			return fmt.Errorf("workloadschema: the $dynamicAnchor %q is declared %d times in a schema with a $dynamicRef", a, n)
		}
	}
	return nil
}

// regexInsts is the instructions of a regular expression as Go compiles it,
// which matching a string runs for each of its bytes at worst.
func regexInsts(expr string) int {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return len(expr) // the compiler refused what does not parse
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return len(expr)
	}
	return len(prog.Inst)
}

// typeMask is the mask of a JSON type's name.
func typeMask(name string) int {
	switch name {
	case "null":
		return nullType
	case "boolean":
		return booleanType
	case "number":
		return numberType
	case "integer":
		return integerType
	case "string":
		return stringType
	case "array":
		return arrayType
	case "object":
		return objectType
	}
	return 0
}

// accepts reports whether a value has a type of the mask. An integer type
// accepts every number: the estimate does not tell 1 from 1.5.
func accepts(mask int, v any) bool {
	var t int
	switch v.(type) {
	case nil:
		t = nullType
	case bool:
		t = booleanType
	case json.Number:
		t = numberType | integerType
	case string:
		t = stringType
	case []any:
		t = arrayType
	case map[string]any:
		t = objectType
	default:
		return true
	}
	return mask&t != 0
}

// compareCost is the steps of comparing a value of a body with v: math/big
// parses a number on each comparison, and an object or an array is compared
// value by value.
func compareCost(v any) int {
	switch v := v.(type) {
	case string, bool, nil:
		return 2
	case map[string]any:
		n := compareSteps
		for _, c := range v {
			n += compareCost(c)
		}
		return n
	case []any:
		n := compareSteps
		for _, c := range v {
			n += compareCost(c)
		}
		return n
	}
	return compareSteps
}

// errTooMuchWork is the end of a walk past maxWork.
var errTooMuchWork = errors.New("too much work")

// work is the walk of a body along a plan, with the steps it has left.
type work struct {
	left int
}

// withinWork reports whether the validator's work on a decoded body stays
// within maxWork. The walk stops at the first step past it, so it takes at
// most that many steps itself.
func (p *plan) withinWork(doc any) bool {
	w := &work{left: maxWork}
	return w.visit(p, doc, 0) == nil
}

// visit counts the evaluation of p at a value at depth levels into the body,
// and all that follows from it. It counts more than the validator does where
// the validator may stop early: it takes every branch of anyOf, oneOf and if,
// every pattern for every member, and every item for contains.
func (w *work) visit(p *plan, v any, depth int) error {
	if w.left -= evalSteps + depth*depthSteps; w.left < 0 {
		return errTooMuchWork
	}
	if p.boolean || (p.types != 0 && !accepts(p.types, v)) {
		return nil
	}
	w.left -= p.compare
	switch v := v.(type) {
	case json.Number:
		if p.numeric {
			w.left -= compareSteps
		}
	case string:
		if p.strings {
			w.left -= len(v) * (1 + 2*p.regex)
		}
	}
	for _, c := range p.same {
		if err := w.visit(c, v, depth); err != nil {
			return err
		}
	}
	switch v := v.(type) {
	case map[string]any:
		if w.left -= len(v) * memberSteps; w.left < 0 {
			return errTooMuchWork
		}
		for name, value := range v {
			w.left -= len(name) * 2 * p.nameRegex
			for _, c := range p.dependent[name] {
				if err := w.visit(c, v, depth); err != nil {
					return err
				}
			}
			if err := w.member(p, name, value, depth+1); err != nil {
				return err
			}
		}
	case []any:
		if p.unique {
			w.left -= len(v) * memberSteps
		}
		for i, item := range v {
			if err := w.item(p, i, item, depth+1); err != nil {
				return err
			}
		}
	}
	if w.left < 0 {
		return errTooMuchWork
	}
	return nil
}

// member counts what p applies to one member of an object.
func (w *work) member(p *plan, name string, value any, depth int) error {
	apply := func(c *plan, v any) error {
		if c == nil {
			return nil
		}
		return w.visit(c, v, depth)
	}
	if c, ok := p.properties[name]; ok {
		if err := apply(c, value); err != nil {
			return err
		}
	} else if err := apply(p.additional, value); err != nil {
		return err
	}
	for _, c := range p.patterns {
		if err := apply(c, value); err != nil {
			return err
		}
	}
	if err := apply(p.unevalProp, value); err != nil {
		return err
	}
	return apply(p.names, name)
}

// item counts what p applies to one item of an array.
func (w *work) item(p *plan, i int, item any, depth int) error {
	apply := func(c *plan) error {
		if c == nil {
			return nil
		}
		return w.visit(c, item, depth)
	}
	if i < len(p.prefix) {
		if err := apply(p.prefix[i]); err != nil {
			return err
		}
	} else if err := apply(p.items); err != nil {
		return err
	}
	if err := apply(p.contains); err != nil {
		return err
	}
	return apply(p.unevalItem)
}

// countValues is the JSON values of a decoded body, counted up to limit.
func countValues(doc any, limit int) int {
	n := 0
	var walk func(any)
	walk = func(v any) {
		if n >= limit {
			return
		}
		n++
		switch v := v.(type) {
		case map[string]any:
			for _, c := range v {
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(doc)
	return n
}
