// Package workloadschema handles the JSON Schema that an agent publishes for a
// workload type: the fingerprint that names it, the compiled schema, and the
// problems a spec body has against it.
//
// A schema comes from an agent over the network and is untrusted. Fingerprint
// and Compile take at most MaxSchemaBytes, and only JSON that RFC 8785 accepts
// whose value is an object. Compile also bounds how deep a schema nests and
// how many subschemas it holds, which the compiler's time grows with. It
// resolves a $ref only inside the schema: a URL anywhere else, remote,
// relative or file, is refused, so compiling fetches nothing and reads no
// file. The JSON Schema metaschemas are the exception: the compiler serves
// them from copies embedded in it.
//
// The validator's work is bounded before it runs (work.go): Compile refuses a
// schema that evaluates more than 1000 subschemas at one location of a body,
// Validate refuses a body over 256 KiB or 10 000 values, and a body whose
// validation the walk along the schema counts past its bound. Validate waits
// for the validator for at most 2 s besides.
package workloadschema

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gowebpki/jcs"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// MaxSchemaBytes bounds one schema, as an agent sends it.
const MaxSchemaBytes = 64 << 10

// The compiler's time grows with the square of a schema's subschemas and with
// the depth they nest at, far within MaxSchemaBytes: 8000 empty subschemas take
// seconds to compile, and so does a chain of 1024 nested ones, while the 9361
// levels that 64 KiB can nest take minutes. Compile refuses a schema past
// either bound below. Within both, the worst shapes compile in tens of
// milliseconds, and the rest of the compiler's time grows with the schema's
// bytes alone: a regular expression of 63 KiB takes about a quarter of a second.
const (
	// maxSchemaDepth bounds how deep the objects and arrays of a schema nest.
	maxSchemaDepth = 32
	// maxSchemaValues bounds the objects and booleans in a schema: each of its
	// subschemas is one of them.
	maxSchemaValues = 512
)

// maxProblems bounds the problems that one validation reports.
const maxProblems = 20

// maxMessageBytes bounds the message of a problem, which may quote a schema's
// enum in full.
const maxMessageBytes = 512

// validationBudget bounds how long Validate waits for the validator: a
// backstop, since the bounds of work.go keep the validator's work far within
// it. A variable, so that tests can shorten it.
var validationBudget = 2 * time.Second

// ErrValidationTooExpensive matches, through errors.Is, the error of a
// validation refused because the walk along the schema counted it past its
// bound, or abandoned because it ran past its budget. Neither affects any
// other validation.
var ErrValidationTooExpensive = errors.New("workloadschema: validation too expensive")

// documentURL is where the compiler holds the schema. A $ref resolves against
// it, or against an $id inside the schema. One that resolves to any other URL
// reaches refuseLoader.
const documentURL = "mem://workload/schema.json"

// errRemoteReference is refuseLoader's answer to every URL.
var errRemoteReference = errors.New("remote reference: a workload schema resolves references only inside itself")

// refuseLoader loads nothing. The compiler asks its loader for every URL that
// is neither in the schema nor a JSON Schema metaschema: a remote or relative
// $ref, a file URL, a $schema that names a metaschema of its own. Without it
// the compiler would read file URLs from the disk.
type refuseLoader struct{}

func (refuseLoader) Load(string) (any, error) {
	return nil, errRemoteReference
}

// printer renders the messages of problems in English, as the library's own
// errors do.
var printer = message.NewPrinter(language.English)

// Schema is a compiled workload schema. It is safe for concurrent use.
type Schema struct {
	compiled *jsonschema.Schema
	plan     *plan
}

// Problem is one way in which a body fails its schema.
type Problem struct {
	// Path is the RFC 6901 JSON pointer of the value that fails: "" is the
	// body itself, and a missing property is a problem of the object that
	// misses it.
	Path string
	// Message says how the value fails.
	Message string
}

// ValidationError is a body that its schema rejects: at most 20 problems,
// sorted by path, array indexes in numeric order.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("body does not match its schema: ")
	for i, p := range e.Problems {
		if i > 0 {
			b.WriteString("; ")
		}
		if p.Path != "" {
			b.WriteString(p.Path)
			b.WriteString(": ")
		}
		b.WriteString(p.Message)
	}
	return b.String()
}

// canonical is the RFC 8785 form of a schema. It refuses raw over
// MaxSchemaBytes, raw that is not JSON (RFC 8785 also refuses duplicate keys,
// invalid Unicode, and numbers beyond an IEEE 754 double), and JSON that is not
// an object.
func canonical(raw []byte) ([]byte, error) {
	if len(raw) > MaxSchemaBytes {
		return nil, fmt.Errorf("workloadschema: schema of %d bytes is over the limit of %d", len(raw), MaxSchemaBytes)
	}
	out, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("workloadschema: schema is not valid JSON: %w", err)
	}
	if len(out) == 0 || out[0] != '{' {
		return nil, errors.New("workloadschema: schema is not a JSON object")
	}
	return out, nil
}

// checkShape refuses a canonical schema whose objects and arrays nest deeper
// than maxSchemaDepth, or that holds more than maxSchemaValues objects and
// booleans. Outside its strings, canonical JSON has a t or an f only where a
// true or a false begins.
func checkShape(c []byte) error {
	depth, values := 0, 0
	inString := false
	for i := 0; i < len(c); i++ {
		if inString {
			switch c[i] {
			case '\\':
				i++ // the escaped byte, which may be a quote
			case '"':
				inString = false
			}
			continue
		}
		switch c[i] {
		case '"':
			inString = true
		case '{', '[':
			if c[i] == '{' {
				values++
			}
			if depth++; depth > maxSchemaDepth {
				return fmt.Errorf("workloadschema: schema nests deeper than %d levels", maxSchemaDepth)
			}
		case '}', ']':
			depth--
		case 't', 'f':
			values++
		}
	}
	if values > maxSchemaValues {
		return fmt.Errorf("workloadschema: schema holds %d objects and booleans, over the limit of %d", values, maxSchemaValues)
	}
	return nil
}

// Fingerprint names a schema: the lowercase hex sha256 of its RFC 8785
// canonical JSON, which key order, whitespace and the spelling of numbers and
// strings do not change. It refuses raw over MaxSchemaBytes, raw that is not
// JSON, and JSON that is not an object.
func Fingerprint(raw []byte) (string, error) {
	c, err := canonical(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

// Compile compiles a schema, as draft 2020-12 unless its $schema names another
// draft. It refuses what Fingerprint refuses, a schema that nests deeper than
// 32 levels or holds more than 512 objects and booleans, a schema that refers
// to any URL outside itself, with a "remote reference" error, and a schema
// that evaluates more than 1000 subschemas at one location of a body or
// refers back to itself there.
//
// It compiles the canonical form, the bytes that the fingerprint hashes, so
// that two schemas with one fingerprint validate alike: each number is the
// IEEE 754 double that RFC 8785 makes of it. That also keeps a number such as
// 1e-999999 out of the compiler, where math/big spends tens of milliseconds on
// each one.
func Compile(raw []byte) (_ *Schema, err error) {
	c, err := canonical(raw)
	if err != nil {
		return nil, err
	}
	if err := checkShape(c); err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(c))
	if err != nil {
		return nil, fmt.Errorf("workloadschema: decode schema: %w", err)
	}
	// The schema is untrusted: a panic of the compiler on it is an error of
	// the schema, not of the process.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("workloadschema: compiler panicked: %v", r)
		}
	}()
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(refuseLoader{})
	if err := compiler.AddResource(documentURL, doc); err != nil {
		return nil, fmt.Errorf("workloadschema: compile schema: %w", err)
	}
	compiled, err := compiler.Compile(documentURL)
	if err != nil {
		return nil, fmt.Errorf("workloadschema: compile schema: %w", err)
	}
	p, err := newPlan(compiled, doc)
	if err != nil {
		return nil, err
	}
	return &Schema{compiled: compiled, plan: p}, nil
}

// Validate checks a body against the schema. It returns nil for a valid body,
// a *ValidationError for a body the schema rejects, a body that is not JSON,
// and a body over 256 KiB or 10 000 values, and an error that matches
// ErrValidationTooExpensive for a validation too expensive to run or to
// finish. Each call validates afresh: none affects another.
//
// The body goes through JSON first and is decoded as the schema was, numbers
// as json.Number, so that the validator sees JSON values whatever Go types the
// caller built the body from (an int, a []string), and each number with the
// precision its encoding has. A nil body is the empty object.
//
// Before the validator runs, Validate walks the body along the schema and
// counts what the validator may do on it; past maxWork, it refuses. The
// validator then runs in a goroutine of its own, and Validate waits for it
// for at most validationBudget, 2 s. Past that, Validate returns the overrun,
// and the abandoned validator runs on in the background until it finishes,
// which the bounds keep within a fraction of a second. It races with nothing:
// it reads the compiled schema, which nothing writes once Compile returns, and
// its own decoding of the body, which nothing else holds, and it leaves its
// result in a buffered channel that no one reads any more.
func (s *Schema) Validate(body map[string]any) error {
	if body == nil {
		body = map[string]any{}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return invalid("body is not JSON: " + err.Error())
	}
	if len(raw) > maxBodyBytes {
		return invalid(fmt.Sprintf("body of %d bytes is over the limit of %d", len(raw), maxBodyBytes))
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return invalid("body cannot be decoded: " + err.Error())
	}
	if countValues(doc, maxBodyValues+1) > maxBodyValues {
		return invalid(fmt.Sprintf("body holds more than %d values", maxBodyValues))
	}
	if !s.plan.withinWork(doc) {
		return fmt.Errorf("%w: this schema would take more than %d steps on this body", ErrValidationTooExpensive, maxWork)
	}
	budget := validationBudget
	done := make(chan error, 1)
	go func() { done <- s.validate(doc) }()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		select {
		case err := <-done: // the validator finished as the budget ran out
			return err
		default:
		}
		return fmt.Errorf("%w: it ran past its budget of %v", ErrValidationTooExpensive, budget)
	}
}

// validate runs the validator over a decoded body and turns what it finds into
// a *ValidationError.
func (s *Schema) validate(doc any) (err error) {
	// The schema is untrusted: a panic of the validator on it rejects the body,
	// and does not take the process down.
	defer func() {
		if r := recover(); r != nil {
			err = invalid(fmt.Sprintf("validator panicked: %v", r))
		}
	}()
	verr := s.compiled.Validate(doc)
	if verr == nil {
		return nil
	}
	var found *jsonschema.ValidationError
	if !errors.As(verr, &found) {
		return invalid(verr.Error())
	}
	return &ValidationError{Problems: problemsOf(found)}
}

// invalid is a *ValidationError of one problem, at the body itself.
func invalid(message string) *ValidationError {
	return &ValidationError{Problems: []Problem{{Path: "", Message: truncate(message)}}}
}

// truncate cuts a message to at most maxMessageBytes, at a rune boundary,
// with an ellipsis for what it cuts.
func truncate(message string) string {
	if len(message) <= maxMessageBytes {
		return message
	}
	const ellipsis = "…"
	cut := maxMessageBytes - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + ellipsis
}

// located is a problem with the location that its path encodes.
type located struct {
	location []string
	Problem
}

// problemsOf flattens what the validator found into problems: an error of a
// kind that only gathers others gives way to its causes, every other error is
// one problem, and a problem found twice counts once. They are sorted by path,
// then by message, and the first maxProblems are kept.
func problemsOf(found *jsonschema.ValidationError) []Problem {
	var all []located
	seen := map[Problem]bool{}
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 && gathers(e.ErrorKind) {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		p := Problem{Path: pointer(e.InstanceLocation), Message: truncate(e.ErrorKind.LocalizedString(printer))}
		if !seen[p] {
			seen[p] = true
			all = append(all, located{e.InstanceLocation, p})
		}
	}
	walk(found)
	if len(all) == 0 {
		return []Problem{{Path: "", Message: found.Error()}}
	}
	slices.SortFunc(all, func(a, b located) int {
		if c := compareLocations(a.location, b.location); c != 0 {
			return c
		}
		return strings.Compare(a.Message, b.Message)
	})
	out := make([]Problem, min(len(all), maxProblems))
	for i := range out {
		out[i] = all[i].Problem
	}
	return out
}

// gathers reports the kinds of error that only gather others: the whole
// validation, a group, a $ref, and allOf, anyOf and oneOf, whose causes say
// what each subschema found. Any other kind is a problem of its own, causes
// or not: the causes of contains are the items that did not match it, and
// those of propertyNames are about a name, not about a value of the body.
func gathers(k jsonschema.ErrorKind) bool {
	switch k.(type) {
	case *kind.Schema, *kind.Group, *kind.Reference, *kind.AllOf, *kind.AnyOf, *kind.OneOf:
		return true
	}
	return false
}

// pointer is the RFC 6901 JSON pointer of a location in the body.
func pointer(location []string) string {
	var b strings.Builder
	for _, token := range location {
		b.WriteByte('/')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

// compareLocations orders locations token by token, and a location before the
// locations inside it.
func compareLocations(a, b []string) int {
	for i := range min(len(a), len(b)) {
		if c := compareTokens(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

// compareTokens orders tokens of digits, array indexes, before the others and
// by number, so that /ports/2 comes before /ports/10. Other tokens compare as
// strings.
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
