// Package workloadschema handles the JSON Schema that an agent publishes for a
// workload type: the fingerprint that names it, the compiled schema, and the
// problems a spec body has against it.
//
// A schema comes from an agent over the network, and a body from a REST user:
// both are untrusted. Each is bounded before anything reads it, and the work
// between them is bounded as it is done:
//
//   - Fingerprint and Compile take at most MaxSchemaBytes, and only JSON that
//     RFC 8785 accepts whose value is an object. Compile also refuses a schema
//     that nests deeper than 32 levels or holds more than 512 objects and
//     booleans, and bounds each regular expression before Go parses it, and
//     again before Go compiles it (regex.go). A compiled subschema keeps no
//     location, and an error of Compile says at most 512 bytes.
//   - Compile accepts draft 2020-12 in a subset, and refuses the rest by name
//     and location (compile.go). A $ref resolves only inside the schema, so
//     compiling fetches nothing and reads no file.
//   - Validate refuses a body over 256 KiB or 10 000 values, or with a number
//     of more than 100 digits or an exponent outside ±400 (value.go). Then it
//     evaluates the body under a budget of steps, which each evaluation, each
//     loop and each leaf charges as it runs; past the budget, it stops at
//     once and returns an error that matches ErrValidationTooExpensive
//     (eval.go).
package workloadschema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gowebpki/jcs"
)

// MaxSchemaBytes bounds one schema, as an agent sends it.
const MaxSchemaBytes = 64 << 10

// The bounds on a schema's shape, which the compiled schema and its regular
// expressions stay within: no more than 512 subschemas, each an object or a
// boolean of the schema.
const (
	// maxSchemaDepth bounds how deep the objects and arrays of a schema nest.
	maxSchemaDepth = 32
	// maxSchemaValues bounds the objects and booleans in a schema.
	maxSchemaValues = 512
)

// maxProblems bounds the problems that one validation reports.
const maxProblems = 20

// maxMessageBytes bounds the message and the path of a problem: a message may
// quote a schema's enum, and a path a body's member names.
const maxMessageBytes = 512

// quoteBytes bounds the bytes of a string that a message quotes.
const quoteBytes = 128

// ErrValidationTooExpensive matches, through errors.Is, the error of a
// validation that ran past its budget of steps, or nested its evaluations too
// deep: the schema and the body together are too costly to check, whichever
// of them makes them so, and the body is neither accepted nor rejected. The
// validation affects no other.
var ErrValidationTooExpensive = errors.New("workloadschema: validation too expensive")

// Schema is a compiled workload schema. It is safe for concurrent use.
type Schema struct {
	root *schema
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

// Compile compiles a workload schema: draft 2020-12, in the subset that
// compile.go lists. It refuses what Fingerprint refuses, a schema past the
// bounds on its shape or on its regular expressions, a keyword outside the
// subset, a keyword whose value is not what it takes, a $ref to anything
// outside the schema, with a "remote reference" error, and a cycle of
// subschemas at one location of a body. Its error says at most 512 bytes:
// the names and values of a schema reach it, and Podium logs it.
//
// It compiles the canonical form, the bytes that the fingerprint hashes, so
// that two schemas with one fingerprint validate alike: each number is the
// IEEE 754 double that RFC 8785 makes of it.
func Compile(raw []byte) (*Schema, error) {
	s, err := compileSchema(raw)
	if err != nil && len(err.Error()) > maxMessageBytes {
		return nil, errors.New(truncate(err.Error()))
	}
	return s, err
}

func compileSchema(raw []byte) (_ *Schema, err error) {
	c, err := canonical(raw)
	if err != nil {
		return nil, err
	}
	if err := checkShape(c); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(c))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("workloadschema: decode schema: %w", err)
	}
	// The schema is untrusted: a panic of the compiler on it, which no test
	// knows of, is an error of the schema, not of the process.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("workloadschema: compiler panicked: %v", r)
		}
	}()
	root, err := compileDocument(doc)
	if err != nil {
		return nil, err
	}
	return &Schema{root: root}, nil
}

// Validate checks a body against the schema. It returns nil for a valid body;
// a *ValidationError for a body the schema rejects, a body that is not JSON,
// and a body past its bounds; and an error that matches
// ErrValidationTooExpensive for a validation that runs past its budget of
// steps or nests too deep. Each call validates afresh: none affects another,
// and none leaves anything running.
//
// The body goes through JSON first, so that the evaluator reads JSON values
// whatever Go types the caller built the body from (an int, a []string), and
// each number exactly as its encoding writes it. A nil body is the empty
// object.
func (s *Schema) Validate(body map[string]any) (err error) {
	// The schema and the body are untrusted: a panic of the evaluator on them,
	// which no test knows of, rejects the body, and does not take the process
	// down.
	defer func() {
		if r := recover(); r != nil {
			err = invalid(fmt.Sprintf("validator panicked: %v", r))
		}
	}()
	doc, verr := decodeBody(body)
	if verr != nil {
		return verr
	}
	return s.root.validate(doc, maxSteps)
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

// quote writes a string for a message: in single quotes, escaped as Go
// escapes it, and cut to quoteBytes first, with an ellipsis for what it cuts.
func quote(s string) string {
	cut := len(s) > quoteBytes
	if cut {
		n := quoteBytes
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	q := strconv.Quote(s)
	q = strings.ReplaceAll(q[1:len(q)-1], `\"`, `"`)
	q = strings.ReplaceAll(q, `'`, `\'`)
	if cut {
		q += "…"
	}
	return "'" + q + "'"
}
