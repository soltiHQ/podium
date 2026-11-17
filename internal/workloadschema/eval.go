package workloadschema

import (
	"cmp"
	"fmt"
	"math/bits"
	"slices"
	"strconv"
	"strings"
)

// The evaluator's work is bounded as it is done, by a budget of steps that
// every part of it charges before doing its work: each evaluation of a
// subschema at a value, each iteration of each loop (items, members, names,
// branches, enum values), and each leaf by its worst case (a regular
// expression by the bytes of the string times the instructions of its
// program, a comparison of values by their weight, a division by the size of
// its numbers). Past the budget, or past maxEvalDepth evaluations nested in
// one another, the evaluator stops at once and Validate returns the
// too-expensive error. Nothing runs on once Validate returns: no goroutine,
// no timer.
//
// The keywords that only decide which subschemas apply, anyOf, oneOf, not, if
// and contains, and propertyNames, which validates names, evaluate their
// subschemas in decision mode: it reports no problem and ends at the first
// failure. Problems are reported in collect mode, on the main path, where a
// failure fails the body; the evaluator stops at maxProblems of them.

// maxSteps is the budget of one validation. A step is at most about a
// nanosecond of the work that charges it (see below), so the budget ends the
// worst validation within the bounds of a schema and a body after a few tens
// of milliseconds. A body of 10 000 values that a usual schema evaluates
// once or twice each takes under a fiftieth of it.
const maxSteps = 25_000_000

// maxEvalDepth bounds the evaluations nested in one another. A body nests
// two or three of them a level, through a property and a $ref.
const maxEvalDepth = 1_000

// What each kind of work costs, in steps. Each was measured on the worst
// input of its kind that the bounds allow, without the race detector, under
// the load of other builds on the same machine: no kind takes much more than
// a nanosecond a step there (the report of Task 4's fix round 3 has the
// table).
const (
	stepsPerEval          = 32   // an evaluation of a subschema at a value,
	stepsPerKeyword       = 2    // and each keyword the subschema holds
	stepsPerItem          = 4    // an iteration of a loop
	stepsPerLookup        = 24   // finding a name in a schema's properties or in a long enum,
	stepsPerHalving       = 16   // or a step of a binary search among the members of an object
	stepsPerMatch         = 128  // matching a regular expression,
	stepsPerMatchedByte   = 6    // and each byte of the string times each instruction of the program
	stepsPerCompare       = 8    // comparing two values,
	stepsPerWeight        = 24   // and each unit of their weight
	stepsPerUniqueItem    = 64   // putting an item into the set of an array's items
	stepsPerName          = 192  // making a member name a value, for propertyNames
	stepsPerSmallDivision = 64   // a multipleOf of numbers of 19 digits or fewer,
	stepsPerBigDivision   = 4096 // or of longer ones, with math/big
	stepsPerProblem       = 4096 // reporting a problem
)

// lookupSteps is what finding a name of n bytes among the m members of an
// object costs: a binary search, each step comparing the name.
func lookupSteps(m, n int) int {
	return (bits.Len(uint(m)) + 1) * (stepsPerHalving + n/8)
}

// pathStep is a step of a location in a body: a member's name, or an item's
// index.
type pathStep struct {
	name  string
	index int // the index of an item; -1 for a member
}

// found is a problem, at the location of the value that has it.
type found struct {
	at      []pathStep
	message string
}

// evaluator evaluates one body against one schema.
type evaluator struct {
	left      int        // the steps left of the budget
	depth     int        // the evaluations in progress
	stopped   bool       // past the budget or the depth, or at maxProblems problems: every evaluation ends
	exhausted bool       // past the budget
	tooDeep   bool       // past maxEvalDepth
	path      []pathStep // the location of the value evaluated in collect mode
	problems  []found
}

// validate evaluates a body against the schema, within a budget of steps.
func (s *schema) validate(body *value, budget int) error {
	e := &evaluator{left: budget}
	ok := e.eval(s, body, true)
	switch {
	case e.exhausted:
		return fmt.Errorf("%w: it takes more than %d steps", ErrValidationTooExpensive, budget)
	case e.tooDeep:
		return fmt.Errorf("%w: it nests more than %d evaluations deep", ErrValidationTooExpensive, maxEvalDepth)
	case len(e.problems) > 0:
		return e.validationError()
	case !ok:
		// Not reached: each failure on the main path reports a problem.
		return invalid("body does not match its schema")
	}
	return nil
}

// charge takes steps from the budget. Past it, or once the evaluator stopped,
// it returns false, and the caller returns at once.
func (e *evaluator) charge(steps int) bool {
	if e.stopped {
		return false
	}
	if e.left -= steps; e.left < 0 {
		e.stopped, e.exhausted = true, true
		return false
	}
	return true
}

// end reports whether an evaluation that just found a failure ends: in
// decision mode it does, and in collect mode once the evaluator stopped.
func (e *evaluator) end(collect bool) bool {
	return !collect || e.stopped
}

// eval evaluates the subschema s at the value v, and reports whether v is
// valid. In collect mode, it reports each problem it finds and goes on; in
// decision mode, it reports none and ends at the first failure. Once the
// evaluator stopped, it returns false, which means nothing.
func (e *evaluator) eval(s *schema, v *value, collect bool) bool {
	if !e.charge(s.cost) {
		return false
	}
	if e.depth == maxEvalDepth {
		e.stopped, e.tooDeep = true, true
		return false
	}
	e.depth++
	ok := e.apply(s, v, collect)
	e.depth--
	return ok
}

// apply evaluates the keywords of s at v: the assertions, then the
// applicators at v, then those at its items or members.
func (e *evaluator) apply(s *schema, v *value, collect bool) bool {
	if s.boolean {
		if !s.valid && collect {
			e.report("false schema")
		}
		return s.valid
	}
	ok := true
	if s.types != 0 && !accepts(s.types, v) {
		if e.end(collect) || e.report("got "+v.kind.String()+", want "+s.typesText) {
			return false
		}
		ok = false
	}
	if s.enum != nil && !e.inEnum(s, v) {
		if e.end(collect) || e.report(s.enumText) {
			return false
		}
		ok = false
	}
	if s.constant != nil && !e.equal(s.constant, v) {
		if e.end(collect) || e.report(s.constText) {
			return false
		}
		ok = false
	}
	held := true
	switch v.kind {
	case numberKind:
		held = !s.readsNumber || e.number(s, v, collect)
	case stringKind:
		held = !s.readsString || e.text(s, v, collect)
	case arrayKind:
		held = !s.readsArray || e.arrayAssertions(s, v, collect)
	case objectKind:
		held = !s.readsObject || e.objectAssertions(s, v, collect)
	}
	if !held {
		if e.end(collect) {
			return false
		}
		ok = false
	}
	if !e.inPlace(s, v, collect) {
		if e.end(collect) {
			return false
		}
		ok = false
	}
	switch {
	case v.kind == arrayKind && s.appliesToItems:
		held = e.itemsOf(s, v, collect)
	case v.kind == objectKind && s.appliesToMembers:
		held = e.membersOf(s, v, collect)
	default:
		held = true
	}
	return held && ok
}

// accepts reports whether a value has one of the types of a mask.
func accepts(types uint8, v *value) bool {
	if types&kindBits[v.kind] != 0 {
		return true
	}
	return v.kind == numberKind && types&typeInteger != 0 && v.n.isInteger()
}

// inEnum reports whether v is one of the values of the enum of s.
func (e *evaluator) inEnum(s *schema, v *value) bool {
	if s.enumIndex != nil {
		if !e.charge(stepsPerLookup) {
			return false
		}
		for _, i := range s.enumIndex[v.hash] {
			if e.equal(s.enum[i], v) {
				return true
			}
			if e.stopped {
				return false
			}
		}
		return false
	}
	for _, x := range s.enum {
		if !e.charge(stepsPerItem) {
			return false
		}
		if e.equal(x, v) {
			return true
		}
		if e.stopped {
			return false
		}
	}
	return false
}

// equal reports whether two values are equal. Values of other hashes are not,
// at no cost beyond the caller's; values of one hash are compared in full.
func (e *evaluator) equal(a, b *value) bool {
	if a.hash != b.hash || a.kind != b.kind {
		return false
	}
	if !e.charge(stepsPerCompare + min(a.weight, b.weight)*stepsPerWeight) {
		return false
	}
	return same(a, b)
}

// number evaluates the keywords of s that read a number.
func (e *evaluator) number(s *schema, v *value, collect bool) bool {
	ok := true
	n := v.n
	steps := stepsPerCompare + len(n.d)/16
	if s.maximum != nil {
		if !e.charge(steps) {
			return false
		}
		if n.compare(*s.maximum) > 0 {
			if e.end(collect) || e.report(boundText("maximum", n, *s.maximum)) {
				return false
			}
			ok = false
		}
	}
	if s.exclusiveMaximum != nil {
		if !e.charge(steps) {
			return false
		}
		if n.compare(*s.exclusiveMaximum) >= 0 {
			if e.end(collect) || e.report(boundText("exclusiveMaximum", n, *s.exclusiveMaximum)) {
				return false
			}
			ok = false
		}
	}
	if s.minimum != nil {
		if !e.charge(steps) {
			return false
		}
		if n.compare(*s.minimum) < 0 {
			if e.end(collect) || e.report(boundText("minimum", n, *s.minimum)) {
				return false
			}
			ok = false
		}
	}
	if s.exclusiveMinimum != nil {
		if !e.charge(steps) {
			return false
		}
		if n.compare(*s.exclusiveMinimum) <= 0 {
			if e.end(collect) || e.report(boundText("exclusiveMinimum", n, *s.exclusiveMinimum)) {
				return false
			}
			ok = false
		}
	}
	if s.multipleOf != nil {
		if !e.charge(s.multipleOf.steps(n)) {
			return false
		}
		if !s.multipleOf.divides(n) {
			if e.end(collect) || e.report(boundText("multipleOf", n, s.multipleOf.n)) {
				return false
			}
			ok = false
		}
	}
	return ok
}

func boundText(keyword string, got, want num) string {
	return keyword + ": got " + got.String() + ", want " + want.String()
}

// text evaluates the keywords of s that read a string.
func (e *evaluator) text(s *schema, v *value, collect bool) bool {
	ok := true
	if s.minLength >= 0 && v.runes < s.minLength {
		if e.end(collect) || e.report(fmt.Sprintf("minLength: got %d, want %d", v.runes, s.minLength)) {
			return false
		}
		ok = false
	}
	if s.maxLength >= 0 && v.runes > s.maxLength {
		if e.end(collect) || e.report(fmt.Sprintf("maxLength: got %d, want %d", v.runes, s.maxLength)) {
			return false
		}
		ok = false
	}
	if s.pattern != nil {
		if !e.charge(s.pattern.steps(len(v.s))) {
			return false
		}
		if !s.pattern.re.MatchString(v.s) {
			if e.end(collect) || e.report(quote(v.s)+" does not match pattern "+quote(s.pattern.text)) {
				return false
			}
			ok = false
		}
	}
	return ok
}

// arrayAssertions evaluates the keywords of s that read an array as a whole.
func (e *evaluator) arrayAssertions(s *schema, v *value, collect bool) bool {
	ok := true
	n := len(v.items)
	if s.minItems >= 0 && n < s.minItems {
		if e.end(collect) || e.report(fmt.Sprintf("minItems: got %d, want %d", n, s.minItems)) {
			return false
		}
		ok = false
	}
	if s.maxItems >= 0 && n > s.maxItems {
		if e.end(collect) || e.report(fmt.Sprintf("maxItems: got %d, want %d", n, s.maxItems)) {
			return false
		}
		ok = false
	}
	if s.uniqueItems && !e.unique(v) {
		if e.end(collect) || e.report(fmt.Sprintf("items at %d and %d are equal", v.dup[0], v.dup[1])) {
			return false
		}
		ok = false
	}
	return ok
}

// unique reports whether the items of an array are unique. It finds out once
// for each array of a body, by hashing its items into a set, and comparing
// in full only items of one hash.
func (e *evaluator) unique(v *value) bool {
	if v.unique != 0 {
		return v.unique == 1
	}
	if !e.charge(len(v.items) * stepsPerUniqueItem) {
		return false
	}
	first := make(map[uint64]int, len(v.items)) // the first item of each hash
	for i, item := range v.items {
		j, seen := first[item.hash]
		if !seen {
			first[item.hash] = i
			continue
		}
		for k := j; k < i; k++ { // the items since the first of that hash, which two unequal values share at times
			if !e.charge(stepsPerItem) {
				return false
			}
			if e.equal(v.items[k], item) {
				v.unique, v.dup = 2, [2]int{k, i}
				return false
			}
			if e.stopped {
				return false
			}
		}
	}
	v.unique = 1
	return true
}

// objectAssertions evaluates the keywords of s that read an object as a whole.
func (e *evaluator) objectAssertions(s *schema, v *value, collect bool) bool {
	ok := true
	m := len(v.keys)
	if s.minProperties >= 0 && m < s.minProperties {
		if e.end(collect) || e.report(fmt.Sprintf("minProperties: got %d, want %d", m, s.minProperties)) {
			return false
		}
		ok = false
	}
	if s.maxProperties >= 0 && m > s.maxProperties {
		if e.end(collect) || e.report(fmt.Sprintf("maxProperties: got %d, want %d", m, s.maxProperties)) {
			return false
		}
		ok = false
	}
	if s.required != nil {
		var missing nameList
		for _, name := range s.required {
			if !e.charge(lookupSteps(m, len(name))) {
				return false
			}
			if _, found := v.member(name); found {
				continue
			}
			if !collect {
				return false
			}
			missing.add(name)
		}
		if missing.count > 0 {
			ok = false
			if e.report(missing.text("missing property", "missing properties", "")) {
				return false
			}
		}
	}
	for _, d := range s.dependentRequired {
		if !e.charge(lookupSteps(m, len(d.name))) {
			return false
		}
		if _, found := v.member(d.name); !found {
			continue
		}
		var missing nameList
		for _, name := range d.names {
			if !e.charge(lookupSteps(m, len(name))) {
				return false
			}
			if _, found := v.member(name); found {
				continue
			}
			if !collect {
				return false
			}
			missing.add(name)
		}
		if missing.count > 0 {
			ok = false
			if e.report(missing.text("property", "properties", "required, if "+quote(d.name)+" exists")) {
				return false
			}
		}
	}
	return ok
}

// inPlace evaluates the applicators of s at v itself.
func (e *evaluator) inPlace(s *schema, v *value, collect bool) bool {
	ok := true
	if s.ref != nil && !e.eval(s.ref, v, collect) {
		if e.end(collect) {
			return false
		}
		ok = false
	}
	for _, sub := range s.allOf {
		if !e.charge(stepsPerItem) {
			return false
		}
		if !e.eval(sub, v, collect) {
			if e.end(collect) {
				return false
			}
			ok = false
		}
	}
	if s.anyOf != nil {
		matched := false
		for _, sub := range s.anyOf {
			if !e.charge(stepsPerItem) {
				return false
			}
			if e.eval(sub, v, false) {
				matched = true
				break
			}
			if e.stopped {
				return false
			}
		}
		if !matched {
			if e.end(collect) || e.report("'anyOf' failed, none matched") {
				return false
			}
			ok = false
		}
	}
	if s.oneOf != nil {
		first, second := -1, -1
		for i, sub := range s.oneOf {
			if !e.charge(stepsPerItem) {
				return false
			}
			if e.eval(sub, v, false) {
				if first >= 0 {
					second = i
					break
				}
				first = i
			}
			if e.stopped {
				return false
			}
		}
		if first < 0 || second >= 0 {
			if e.end(collect) || e.report(oneOfText(first, second)) {
				return false
			}
			ok = false
		}
	}
	if s.not != nil {
		matched := e.eval(s.not, v, false)
		if e.stopped {
			return false
		}
		if matched {
			if e.end(collect) || e.report("'not' failed") {
				return false
			}
			ok = false
		}
	}
	if s.ifSchema != nil {
		matched := e.eval(s.ifSchema, v, false)
		if e.stopped {
			return false
		}
		branch := s.elseSchema
		if matched {
			branch = s.thenSchema
		}
		if branch != nil && !e.eval(branch, v, collect) {
			if e.end(collect) {
				return false
			}
			ok = false
		}
	}
	return ok
}

func oneOfText(first, second int) string {
	if first < 0 {
		return "'oneOf' failed, none matched"
	}
	return fmt.Sprintf("'oneOf' failed, subschemas %d, %d matched", first, second)
}

// itemsOf evaluates the applicators of s at the items of an array.
func (e *evaluator) itemsOf(s *schema, v *value, collect bool) bool {
	ok := true
	n := len(v.items)
	for i, sub := range s.prefixItems {
		if i >= n {
			break
		}
		if !e.charge(stepsPerItem) {
			return false
		}
		if !e.child(sub, v.items[i], pathStep{index: i}, collect) {
			if e.end(collect) {
				return false
			}
			ok = false
		}
	}
	if s.items != nil && n > len(s.prefixItems) {
		if s.itemsFalse {
			if e.end(collect) || e.report(fmt.Sprintf("items: got %d, want at most %d", n, len(s.prefixItems))) {
				return false
			}
			ok = false
		} else {
			for i := len(s.prefixItems); i < n; i++ {
				if !e.charge(stepsPerItem) {
					return false
				}
				if !e.child(s.items, v.items[i], pathStep{index: i}, collect) {
					if e.end(collect) {
						return false
					}
					ok = false
				}
			}
		}
	}
	if s.contains != nil && !e.containsHolds(s, v, collect) {
		if e.end(collect) {
			return false
		}
		ok = false
	}
	return ok
}

// containsHolds evaluates contains, minContains and maxContains: it counts the
// items that contains matches, until the count decides.
func (e *evaluator) containsHolds(s *schema, v *value, collect bool) bool {
	matched := 0
	for _, item := range v.items {
		if s.maxContains < 0 && matched >= s.minContains {
			break
		}
		if !e.charge(stepsPerItem) {
			return false
		}
		if e.eval(s.contains, item, false) {
			if matched++; s.maxContains >= 0 && matched > s.maxContains {
				break
			}
		}
		if e.stopped {
			return false
		}
	}
	switch {
	case matched < s.minContains:
		if collect {
			if matched == 0 && s.minContains == 1 {
				e.report("no items match contains schema")
			} else {
				e.report(fmt.Sprintf("min %d items required to match contains schema, but matched %d", s.minContains, matched))
			}
		}
		return false
	case s.maxContains >= 0 && matched > s.maxContains:
		if collect {
			e.report(fmt.Sprintf("max %d items allowed to match contains schema, but matched more", s.maxContains))
		}
		return false
	}
	return true
}

// membersOf evaluates the applicators of s at the members of an object, and
// dependentSchemas, at the object itself.
func (e *evaluator) membersOf(s *schema, v *value, collect bool) bool {
	ok := true
	m := len(v.keys)
	for _, d := range s.dependentSchemas {
		if !e.charge(lookupSteps(m, len(d.name))) {
			return false
		}
		if _, found := v.member(d.name); found && !e.eval(d.schema, v, collect) {
			if e.end(collect) {
				return false
			}
			ok = false
		}
	}
	if s.propertyNames != nil {
		names := e.nameValues(v)
		if e.stopped {
			return false
		}
		for i := range names {
			if !e.charge(stepsPerItem) {
				return false
			}
			if !e.eval(s.propertyNames, &names[i], false) {
				if e.end(collect) || e.report("invalid propertyName "+quote(v.keys[i])) {
					return false
				}
				ok = false
			}
		}
	}
	if s.patternProperties == nil && s.additionalProperties == nil {
		if s.properties != nil && !e.propertiesOnly(s, v, collect) {
			if e.end(collect) {
				return false
			}
			ok = false
		}
		return ok
	}
	var extra nameList // the members that additionalProperties false refuses
	for i, name := range v.keys {
		if !e.charge(stepsPerItem) {
			return false
		}
		at := pathStep{name: name, index: -1}
		matched := false
		if s.properties != nil {
			if !e.charge(stepsPerLookup + len(name)/4) {
				return false
			}
			if sub, found := s.properties[name]; found {
				matched = true
				if !e.child(sub, v.vals[i], at, collect) {
					if e.end(collect) {
						return false
					}
					ok = false
				}
			}
		}
		for _, pp := range s.patternProperties {
			if !e.charge(pp.pattern.steps(len(name))) {
				return false
			}
			if !pp.pattern.re.MatchString(name) {
				continue
			}
			matched = true
			if !e.child(pp.schema, v.vals[i], at, collect) {
				if e.end(collect) {
					return false
				}
				ok = false
			}
		}
		switch {
		case matched || s.additionalProperties == nil:
		case s.additionalFalse:
			if !collect {
				return false
			}
			ok = false
			extra.add(name)
		case !e.child(s.additionalProperties, v.vals[i], at, collect):
			if e.end(collect) {
				return false
			}
			ok = false
		}
	}
	if extra.count > 0 && e.report(extra.text("additional property", "additional properties", "not allowed")) {
		return false
	}
	return ok
}

// propertiesOnly evaluates properties without patternProperties or
// additionalProperties, from the fewer of the schema's properties and the
// object's members.
func (e *evaluator) propertiesOnly(s *schema, v *value, collect bool) bool {
	ok := true
	m := len(v.keys)
	if len(s.propertyList) <= m {
		for _, p := range s.propertyList {
			if !e.charge(lookupSteps(m, len(p.name))) {
				return false
			}
			i, found := v.member(p.name)
			if found && !e.child(p.schema, v.vals[i], pathStep{name: p.name, index: -1}, collect) {
				if e.end(collect) {
					return false
				}
				ok = false
			}
		}
		return ok
	}
	for i, name := range v.keys {
		if !e.charge(stepsPerItem + stepsPerLookup + len(name)/4) {
			return false
		}
		if sub, found := s.properties[name]; found && !e.child(sub, v.vals[i], pathStep{name: name, index: -1}, collect) {
			if e.end(collect) {
				return false
			}
			ok = false
		}
	}
	return ok
}

// nameValues is the member names of an object as string values, which it
// makes once for each object of a body.
func (e *evaluator) nameValues(v *value) []value {
	if v.names != nil || len(v.keys) == 0 {
		return v.names
	}
	names := make([]value, len(v.keys))
	for i, name := range v.keys {
		if !e.charge(stepsPerName + len(name)/2) {
			return nil
		}
		names[i] = stringValue(name)
	}
	v.names = names
	return names
}

// child evaluates s at an item or a member of the value at e.path.
func (e *evaluator) child(s *schema, v *value, at pathStep, collect bool) bool {
	if !collect {
		return e.eval(s, v, false)
	}
	e.path = append(e.path, at)
	ok := e.eval(s, v, true)
	e.path = e.path[:len(e.path)-1]
	return ok
}

// report records a problem of the value at e.path, unless it has it already,
// and reports whether the evaluator stopped: past the budget, or at
// maxProblems problems.
func (e *evaluator) report(message string) bool {
	message = truncate(message)
	if !e.charge(stepsPerProblem + (len(e.problems)+1)*(len(e.path)+len(message)/16)) {
		return true
	}
	for _, p := range e.problems {
		if p.message == message && slices.Equal(p.at, e.path) {
			return false
		}
	}
	e.problems = append(e.problems, found{at: slices.Clone(e.path), message: message})
	if len(e.problems) == maxProblems {
		e.stopped = true
	}
	return e.stopped
}

// validationError is the problems found, sorted by path, then by message.
func (e *evaluator) validationError() *ValidationError {
	slices.SortFunc(e.problems, func(a, b found) int {
		if c := compareLocations(a.at, b.at); c != 0 {
			return c
		}
		return strings.Compare(a.message, b.message)
	})
	out := make([]Problem, len(e.problems))
	for i, p := range e.problems {
		out[i] = Problem{Path: pointer(p.at), Message: p.message}
	}
	return &ValidationError{Problems: out}
}

// nameList is the names a problem lists, which stops listing once the
// problem is long enough to be cut.
type nameList struct {
	names []string
	count int
	bytes int
}

func (l *nameList) add(name string) {
	l.count++
	if l.bytes <= maxMessageBytes {
		l.names = append(l.names, name)
		l.bytes += min(len(name), quoteBytes) + 4
	}
}

// text is the problem: "missing property 'a'", "missing properties 'a', 'b'".
func (l *nameList) text(one, many, after string) string {
	var b strings.Builder
	if l.count == 1 {
		b.WriteString(one)
	} else {
		b.WriteString(many)
	}
	for i, name := range l.names {
		if i == 0 {
			b.WriteByte(' ')
		} else {
			b.WriteString(", ")
		}
		b.WriteString(quote(name))
	}
	if len(l.names) < l.count {
		b.WriteString(", …")
	}
	if after != "" {
		b.WriteByte(' ')
		b.WriteString(after)
	}
	return b.String()
}

// pointer is the RFC 6901 JSON pointer of a location in a body, cut to
// maxMessageBytes.
func pointer(at []pathStep) string {
	var b strings.Builder
	for _, p := range at {
		if b.Len() > maxMessageBytes {
			break
		}
		b.WriteByte('/')
		if p.index >= 0 {
			b.WriteString(strconv.Itoa(p.index))
			continue
		}
		name := p.name
		if len(name) > maxMessageBytes {
			name = name[:maxMessageBytes+1] // past the cut, which truncate makes at a rune boundary
		}
		b.WriteString(escapeToken(name))
	}
	return truncate(b.String())
}

// compareLocations orders locations step by step, and a location before the
// locations inside it.
func compareLocations(a, b []pathStep) int {
	for i := range min(len(a), len(b)) {
		if c := compareSteps(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

// compareSteps orders array indexes by number, and member names of digits
// alike, before other names, which compare as strings: /ports/2 comes before
// /ports/10.
func compareSteps(a, b pathStep) int {
	if a.index >= 0 && b.index >= 0 {
		return cmp.Compare(a.index, b.index)
	}
	return compareTokens(a.token(), b.token())
}

func (p pathStep) token() string {
	if p.index >= 0 {
		return strconv.Itoa(p.index)
	}
	return p.name
}

// compareTokens orders tokens of digits before the others, and by number.
// Other tokens compare as strings.
func compareTokens(a, b string) int {
	ai, bi := isIndex(a), isIndex(b)
	switch {
	case ai && bi:
		if c := cmp.Compare(len(a), len(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	case ai:
		return -1
	case bi:
		return 1
	}
	return strings.Compare(a, b)
}

// isIndex reports a token of decimal digits only.
func isIndex(token string) bool {
	if token == "" {
		return false
	}
	for i := range len(token) {
		if token[i] < '0' || token[i] > '9' {
			return false
		}
	}
	return true
}
