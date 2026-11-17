package workloadschema

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"maps"
	"math/big"
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The bounds of a body, which Validate checks before it evaluates anything.
const (
	// maxBodyBytes bounds a body as JSON, encoded without HTML escaping.
	maxBodyBytes = 256 << 10
	// maxBodyValues bounds the JSON values of a body: objects, arrays and
	// scalars alike.
	maxBodyValues = 10_000
)

// The bounds of a number, in a body or in a schema, which keep the arithmetic
// on numbers bounded: no more than maxNumberDigits digits, and an exponent
// within ±maxNumberExponent. A schema's numbers are within them always: they
// are IEEE 754 doubles, as RFC 8785 writes them.
const (
	maxNumberDigits   = 100
	maxNumberExponent = 400
)

// kind is the JSON type of a value.
type kind uint8

const (
	nullKind kind = iota
	booleanKind
	numberKind
	stringKind
	arrayKind
	objectKind
)

var kindNames = [...]string{"null", "boolean", "number", "string", "array", "object"}

func (k kind) String() string { return kindNames[k] }

// value is a JSON value of a body, or of a schema's enum or const, in the form
// the evaluator reads: numbers exact, the members of an object sorted by name,
// and a hash that equal values share, however they are spelled and whatever
// order their members come in.
type value struct {
	kind  kind
	b     bool   // a boolean
	n     num    // a number
	s     string // a string
	runes int    // the code points of a string

	items []*value // the items of an array
	keys  []string // the member names of an object, sorted
	vals  []*value // the member values of an object, in the order of keys

	hash uint64
	// weight is what comparing the value with an equal one reads: one for each
	// value it holds, itself included, and one for each 16 bytes of its
	// strings, member names and digits.
	weight int

	// What an evaluation found out about a value of a body, for the next
	// evaluation to read. Only the evaluator of one Validate call writes them;
	// the values of a schema are compared, never evaluated, and stay as
	// Compile made them.
	unique int8    // an array: 0 not known yet, 1 its items are unique, 2 not
	dup    [2]int  // an array whose items are not unique: the first two equal ones
	names  []value // an object: its member names as strings, once propertyNames read them
}

// member is the position of the member of an object that has a name.
func (v *value) member(name string) (int, bool) {
	return slices.BinarySearch(v.keys, name)
}

// hashSeed seeds the hashes of values for the life of the process. The values
// of a compiled schema, which a body's values are compared with, are hashed
// with it too; compiled schemas live in memory only.
var hashSeed = maphash.MakeSeed()

// seal computes the hash and the weight of v, whose items or members are
// sealed already.
func (v *value) seal() {
	var h maphash.Hash
	h.SetSeed(hashSeed)
	_ = h.WriteByte(byte(v.kind))
	v.weight = 1
	switch v.kind {
	case booleanKind:
		if v.b {
			_ = h.WriteByte(1)
		} else {
			_ = h.WriteByte(0)
		}
	case numberKind:
		if v.n.neg {
			_ = h.WriteByte(1)
		} else {
			_ = h.WriteByte(0)
		}
		writeUint(&h, uint64(int64(v.n.e)))
		_, _ = h.WriteString(v.n.d)
		v.weight += len(v.n.d) / 16
	case stringKind:
		_, _ = h.WriteString(v.s)
		v.weight += len(v.s) / 16
	case arrayKind:
		writeUint(&h, uint64(len(v.items)))
		for _, item := range v.items {
			writeUint(&h, item.hash)
			v.weight += item.weight
		}
	case objectKind:
		writeUint(&h, uint64(len(v.keys)))
		for i, name := range v.keys {
			writeUint(&h, uint64(len(name)))
			_, _ = h.WriteString(name)
			writeUint(&h, v.vals[i].hash)
			v.weight += len(name)/16 + v.vals[i].weight
		}
	}
	v.hash = h.Sum64()
}

func writeUint(h *maphash.Hash, x uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], x)
	_, _ = h.Write(b[:])
}

// stringValue is a string as a value.
func stringValue(s string) value {
	v := value{kind: stringKind, s: s, runes: utf8.RuneCountInString(s)}
	v.seal()
	return v
}

// same reports whether two values are equal as JSON values are: numbers by
// their value, so that 1 and 1.0 are equal, and objects whatever the order of
// their members.
func same(a, b *value) bool {
	if a.hash != b.hash || a.kind != b.kind {
		return false
	}
	switch a.kind {
	case booleanKind:
		return a.b == b.b
	case numberKind:
		return a.n == b.n
	case stringKind:
		return a.s == b.s
	case arrayKind:
		if len(a.items) != len(b.items) {
			return false
		}
		for i := range a.items {
			if !same(a.items[i], b.items[i]) {
				return false
			}
		}
	case objectKind:
		if len(a.keys) != len(b.keys) {
			return false
		}
		for i := range a.keys {
			if a.keys[i] != b.keys[i] || !same(a.vals[i], b.vals[i]) {
				return false
			}
		}
	}
	return true
}

// builder makes values out of decoded JSON, whose numbers are json.Number.
type builder struct {
	// slab holds the values of a body, allocated at once; nil allocates each
	// value on its own.
	slab []value
	// at is the location of the value being built, and bad the problem with
	// the first number past the bounds.
	at  []pathStep
	bad *ValidationError
}

func (b *builder) alloc() *value {
	if len(b.slab) < cap(b.slab) {
		b.slab = b.slab[:len(b.slab)+1]
		return &b.slab[len(b.slab)-1]
	}
	return &value{}
}

// build makes the value of x, or records the first number past the bounds in
// bad and returns nil.
func (b *builder) build(x any) *value {
	v := b.alloc()
	switch x := x.(type) {
	case nil:
		v.kind = nullKind
	case bool:
		v.kind, v.b = booleanKind, x
	case json.Number:
		n, err := parseNum(string(x))
		if err != nil {
			b.bad = &ValidationError{Problems: []Problem{{Path: pointer(b.at), Message: err.Error()}}}
			return nil
		}
		v.kind, v.n = numberKind, n
	case string:
		v.kind, v.s, v.runes = stringKind, x, utf8.RuneCountInString(x)
	case []any:
		v.kind = arrayKind
		v.items = make([]*value, len(x))
		for i, item := range x {
			b.at = append(b.at, pathStep{index: i})
			v.items[i] = b.build(item)
			b.at = b.at[:len(b.at)-1]
			if b.bad != nil {
				return nil
			}
		}
	case map[string]any:
		v.kind = objectKind
		v.keys = slices.Sorted(maps.Keys(x))
		v.vals = make([]*value, len(v.keys))
		for i, name := range v.keys {
			b.at = append(b.at, pathStep{name: name, index: -1})
			v.vals[i] = b.build(x[name])
			b.at = b.at[:len(b.at)-1]
			if b.bad != nil {
				return nil
			}
		}
	default:
		// Decoded JSON holds none: a caller could only pass one in a body,
		// which goes through JSON first.
		b.bad = invalid(fmt.Sprintf("body holds a %T, which is not JSON", x))
		return nil
	}
	v.seal()
	return v
}

// decodeBody turns a body into values: through JSON, encoded without HTML
// escaping and decoded with json.Number, so that the evaluator reads JSON
// values whatever Go types the caller built the body from (an int, a
// []string), and each number with the precision its encoding has. It refuses
// a body over maxBodyBytes or maxBodyValues, and a number past the bounds of
// numbers, as a *ValidationError. A nil body is the empty object.
func decodeBody(body map[string]any) (*value, *ValidationError) {
	if body == nil {
		body = map[string]any{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, invalid("body is not JSON: " + err.Error())
	}
	raw := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	if size := jsonSize(raw); size > maxBodyBytes {
		return nil, invalid(fmt.Sprintf("body of %d bytes is over the limit of %d", size, maxBodyBytes))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, invalid("body cannot be decoded: " + err.Error())
	}
	n := countValues(doc, maxBodyValues+1)
	if n > maxBodyValues {
		return nil, invalid(fmt.Sprintf("body holds more than %d values", maxBodyValues))
	}
	b := &builder{slab: make([]value, 0, n)}
	v := b.build(doc)
	if b.bad != nil {
		return nil, b.bad
	}
	return v, nil
}

// jsonSize is the size of JSON that encoding/json wrote, with U+2028 and
// U+2029 counted as their 3 bytes of UTF-8: encoding/json escapes them as
// \u2028 and \u2029, 6 bytes each, which JSON does not need. A backslash
// escapes the byte after it, so a backslash that the data holds, written \\,
// does not start an escape of its own.
func jsonSize(raw []byte) int {
	n := len(raw)
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if bytes.HasPrefix(raw[i+1:], []byte("u2028")) || bytes.HasPrefix(raw[i+1:], []byte("u2029")) {
			n -= 3
		}
		i++ // the escaped byte
	}
	return n
}

// countValues is the JSON values of decoded JSON, counted up to limit.
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

// num is a JSON number, exactly: (-1)^neg × d × 10^e. The digits d have no
// leading or trailing zero, and are "" for zero, which is never negative, so
// that equal numbers are equal structs: 1, 1.0 and 10e-1 alike.
type num struct {
	neg bool
	d   string
	e   int
}

var (
	errNumberDigits   = fmt.Errorf("number has more than %d digits", maxNumberDigits)
	errNumberExponent = fmt.Errorf("number has an exponent outside ±%d", maxNumberExponent)
	errNumberSyntax   = errors.New("not a JSON number")
)

// parseNum reads a JSON number. It refuses one of more than maxNumberDigits
// digits, or whose exponent is outside ±maxNumberExponent.
func parseNum(lit string) (num, error) {
	var n num
	i := 0
	if i < len(lit) && lit[i] == '-' {
		n.neg = true
		i++
	}
	start := i
	for i < len(lit) && isDigit(lit[i]) {
		i++
	}
	whole := lit[start:i]
	if whole == "" {
		return num{}, errNumberSyntax
	}
	var fraction string
	if i < len(lit) && lit[i] == '.' {
		i++
		start = i
		for i < len(lit) && isDigit(lit[i]) {
			i++
		}
		if fraction = lit[start:i]; fraction == "" {
			return num{}, errNumberSyntax
		}
	}
	exp := 0
	if i < len(lit) && (lit[i] == 'e' || lit[i] == 'E') {
		i++
		negative := false
		if i < len(lit) && (lit[i] == '+' || lit[i] == '-') {
			negative = lit[i] == '-'
			i++
		}
		start = i
		for i < len(lit) && lit[i] == '0' {
			i++
		}
		significant := i
		for i < len(lit) && isDigit(lit[i]) {
			i++
		}
		if i == start {
			return num{}, errNumberSyntax
		}
		if i-significant > 3 {
			return num{}, errNumberExponent
		}
		for _, c := range lit[significant:i] {
			exp = exp*10 + int(c-'0')
		}
		if exp > maxNumberExponent {
			return num{}, errNumberExponent
		}
		if negative {
			exp = -exp
		}
	}
	if i != len(lit) {
		return num{}, errNumberSyntax
	}
	if len(whole)+len(fraction) > maxNumberDigits {
		return num{}, errNumberDigits
	}
	digits := strings.TrimLeft(whole+fraction, "0")
	trimmed := strings.TrimRight(digits, "0")
	if trimmed == "" {
		return num{}, nil
	}
	n.d, n.e = trimmed, exp-len(fraction)+len(digits)-len(trimmed)
	return n, nil
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func (a num) sign() int {
	switch {
	case a.d == "":
		return 0
	case a.neg:
		return -1
	}
	return 1
}

// compare orders two numbers by their value.
func (a num) compare(b num) int {
	sa, sb := a.sign(), b.sign()
	if sa != sb || sa == 0 {
		return cmp.Compare(sa, sb)
	}
	// Of two magnitudes, the one whose leading digit has the higher place is
	// the larger; at the same place, the digits decide, and a number whose
	// digits go on past the other's is the larger, for its last digit is not
	// zero.
	c := cmp.Compare(len(a.d)+a.e, len(b.d)+b.e)
	if c == 0 {
		c = strings.Compare(a.d, b.d)
	}
	return sa * c
}

// isInteger reports a number with no fractional part.
func (a num) isInteger() bool { return a.e >= 0 }

// String writes a number as ECMAScript does: plain digits for a magnitude
// from 1e-6 to 1e21, and an exponent beyond.
func (a num) String() string {
	if a.d == "" {
		return "0"
	}
	var b strings.Builder
	if a.neg {
		b.WriteByte('-')
	}
	point := len(a.d) + a.e // where the decimal point goes, counted from the first digit
	switch {
	case a.e >= 0 && point <= 21:
		b.WriteString(a.d)
		b.WriteString(strings.Repeat("0", a.e))
	case a.e < 0 && point > 0:
		b.WriteString(a.d[:point])
		b.WriteByte('.')
		b.WriteString(a.d[point:])
	case a.e < 0 && point > -6:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -point))
		b.WriteString(a.d)
	default:
		b.WriteString(a.d[:1])
		if len(a.d) > 1 {
			b.WriteByte('.')
			b.WriteString(a.d[1:])
		}
		b.WriteByte('e')
		if point > 0 {
			b.WriteByte('+')
		}
		b.WriteString(strconv.Itoa(point - 1))
	}
	return b.String()
}

// multiple is the divisor of a multipleOf, a number over zero.
type multiple struct {
	n     num
	big   *big.Int // its digits
	small uint64   // its digits, when they fit a uint64; 0 when not
}

func newMultiple(n num) *multiple {
	m := &multiple{n: n, big: new(big.Int)}
	m.big.SetString(n.d, 10)
	if len(n.d) <= 19 {
		m.small, _ = strconv.ParseUint(n.d, 10, 64)
	}
	return m
}

// steps is what dividing v by the divisor costs.
func (m *multiple) steps(v num) int64 {
	if m.small != 0 && len(v.d) <= 19 {
		return stepsPerSmallDivision
	}
	return stepsPerBigDivision
}

// divides reports whether v is an integer multiple of the divisor.
//
// With v = dv × 10^ev and the divisor dm × 10^em, the quotient is
// dv × 10^(ev-em) / dm. When ev < em, it is not an integer unless dv is zero:
// dv would have to be a multiple of ten, and its last digit is not zero.
// Otherwise it is one when dm divides dv × 10^(ev-em), which the exponent
// bounds keep within a few thousand bits.
func (m *multiple) divides(v num) bool {
	if v.d == "" {
		return true
	}
	k := v.e - m.n.e
	if k < 0 {
		return false
	}
	if m.small != 0 && len(v.d) <= 19 {
		dv, _ := strconv.ParseUint(v.d, 10, 64)
		hi, lo := bits.Mul64(dv%m.small, powMod(10, k, m.small))
		return bits.Rem64(hi, lo, m.small) == 0
	}
	dv, _ := new(big.Int).SetString(v.d, 10)
	p := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(k)), m.big)
	dv.Mul(dv, p)
	return dv.Mod(dv, m.big).Sign() == 0
}

// powMod is base^exp mod m, for m over zero.
func powMod(base uint64, exp int, m uint64) uint64 {
	result, b := 1%m, base%m
	for ; exp > 0; exp >>= 1 {
		if exp&1 == 1 {
			hi, lo := bits.Mul64(result, b)
			result = bits.Rem64(hi, lo, m)
		}
		hi, lo := bits.Mul64(b, b)
		b = bits.Rem64(hi, lo, m)
	}
	return result
}
