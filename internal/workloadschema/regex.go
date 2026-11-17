package workloadschema

import (
	"errors"
	"fmt"
	"math/bits"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A regular expression of a schema, a pattern or a patternProperties key, is
// bounded three times before Go compiles it, so that neither Go's parser nor
// its compiler does more than the bounds allow:
//
//   - by its source: at most maxPatternBytes, and maxSchemaPatternBytes for
//     all those of a schema;
//   - by what parsing it builds beyond reading it, estimated from its source
//     (patternWork): the ranges of each Unicode class, and the runes of each
//     range that case folding folds one at a time. At most maxPatternWork, and
//     maxSchemaPatternWork for all;
//   - by the size of its program, measured on the parsed expression
//     (regexSize), with each character class counted by its ranges: at most
//     maxRegexSize, and maxSchemaRegexSize for all.
//
// Matching it is charged by its weight (regexWeight), the program as matching
// runs it, which a class enters by a binary search of its ranges.
const (
	maxPatternBytes       = 1 << 10
	maxSchemaPatternBytes = 16 << 10
	maxPatternWork        = 32_000
	maxSchemaPatternWork  = 128_000
	maxRegexSize          = 2_000
	maxSchemaRegexSize    = 20_000
)

// unicodeClassWork is what a Unicode class escape, \p or \P, counts in a
// pattern's work: over what Go's parser builds for the largest Unicode table,
// its ranges, and under case folding its fold table's too, and their merge
// (1 502 for \p{Lu} in Go 1.27). TestPatternWorkCountsWhatParsingBuilds
// checks it against every table.
const unicodeClassWork = 2_048

// Under case folding, Go's parser folds each rune of a range inside brackets
// one at a time, if the range lies partly within minFold..maxFold. Such a
// rune counts a quarter of a range in a pattern's work.
const (
	minFold            = 0x41
	maxFold            = 0x1E943
	foldedRunesPerUnit = 4
)

// pattern is a compiled regular expression.
type pattern struct {
	re     *regexp.Regexp
	weight int // the program as matching runs it, as regexWeight measures it
	text   string
}

// steps is what matching a string of n bytes costs at worst: Go's engines run
// at most the whole program for each byte.
func (p *pattern) steps(n int) int64 {
	return stepsPerMatch + int64(n)*int64(p.weight)*stepsPerMatchedByte
}

// regexBounds is what the regular expressions of one schema have taken of
// their bounds so far.
type regexBounds struct {
	bytes, work, size int
}

// compile bounds a regular expression and compiles it, unless it is past a
// bound, or takes the schema's regular expressions past theirs. The error
// says how, for the keyword that holds the expression.
func (b *regexBounds) compile(text string) (*pattern, error) {
	if len(text) > maxPatternBytes {
		return nil, fmt.Errorf("is a regular expression of %d bytes, over the limit of %d", len(text), maxPatternBytes)
	}
	if b.bytes += len(text); b.bytes > maxSchemaPatternBytes {
		return nil, fmt.Errorf("takes the schema's regular expressions to %d bytes, over the limit of %d in all", b.bytes, maxSchemaPatternBytes)
	}
	work, ok := patternWork(text)
	if !ok {
		return nil, errors.New("may fold case, and has a range inside brackets whose ends Compile cannot read")
	}
	if work > maxPatternWork {
		return nil, fmt.Errorf("is a regular expression whose parsing builds %d ranges, over the limit of %d", work, maxPatternWork)
	}
	if b.work += work; b.work > maxSchemaPatternWork {
		return nil, fmt.Errorf("takes the parsing of the schema's regular expressions to %d ranges, over the limit of %d in all", b.work, maxSchemaPatternWork)
	}
	parsed, err := syntax.Parse(text, syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("is not a valid regular expression: %w", err)
	}
	size := regexSize(parsed)
	if size > maxRegexSize {
		return nil, fmt.Errorf("is a regular expression of %s instructions and ranges, over the limit of %d", sizeText(size), maxRegexSize)
	}
	if b.size += size; b.size > maxSchemaRegexSize {
		return nil, fmt.Errorf("takes the schema's regular expressions to %d instructions and ranges, over the limit of %d in all", b.size, maxSchemaRegexSize)
	}
	re, err := regexp.Compile(text)
	if err != nil {
		return nil, fmt.Errorf("is not a valid regular expression: %w", err)
	}
	return &pattern{re: re, weight: regexWeight(parsed), text: text}, nil
}

// patternWork estimates, from a pattern's source, what Go's parser builds
// beyond reading it: unicodeClassWork for each \p or \P, and, where case
// folding may be on, a quarter of a range for each rune of a bracket range
// that it folds one at a time. It errs high: it counts a \p after an escaped
// backslash or inside \Q...\E, and assumes folding wherever an i flag
// appears. ok is false when folding may be on and it cannot read the ends of
// a range.
func patternWork(src string) (work int, ok bool) {
	work = (strings.Count(src, `\p`) + strings.Count(src, `\P`)) * unicodeClassWork
	if !mayFoldCase(src) {
		return work, true
	}
	runes, ok := foldedRunes(src)
	return work + runes/foldedRunesPerUnit, ok
}

// mayFoldCase reports a pattern with a flag group that names i: (?i), (?i:,
// (?-i), (?im) and the like, anywhere, even where it is not a flag group.
func mayFoldCase(src string) bool {
	for i := 0; i+1 < len(src); i++ {
		if src[i] != '(' || src[i+1] != '?' {
			continue
		}
		j, sawI := i+2, false
		for j < len(src) && strings.IndexByte("imsU-", src[j]) >= 0 {
			sawI = sawI || src[j] == 'i'
			j++
		}
		if sawI && j < len(src) && (src[j] == ')' || src[j] == ':') {
			return true
		}
	}
	return false
}

// foldedRunes counts the runes that Go's parser folds one at a time in the
// ranges inside a pattern's brackets, reading the pattern as the parser does:
// escapes, \Q...\E, and the classes. It reports false where it cannot read
// the ends of a range.
func foldedRunes(src string) (int, bool) {
	n := 0
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], `\Q`):
			end := strings.Index(src[i+2:], `\E`)
			if end < 0 {
				return n, true // literal text to the end
			}
			i += 2 + end + 2
		case src[i] == '\\':
			i = skipEscape(src, i)
		case src[i] == '[':
			m, next, readable := classFolds(src, i)
			if !readable {
				return 0, false
			}
			n += m
			i = next
		default:
			i++
		}
	}
	return n, true
}

// classFolds counts the runes folded one at a time in the class that begins
// at src[i], as Go's parseClass reads it, and returns where the class ends.
// It reports false where it cannot read the ends of a range.
func classFolds(src string, i int) (int, int, bool) {
	n, t := 0, i+1
	if t < len(src) && src[t] == '^' {
		t++
	}
	for first := true; t < len(src) && (src[t] != ']' || first); first = false {
		if strings.HasPrefix(src[t:], "[:") { // [:alpha:]: an ASCII class
			if end := strings.Index(src[t+2:], ":]"); end >= 0 {
				t += 2 + end + 2
				continue
			}
		}
		if src[t] == '\\' && t+1 < len(src) && strings.IndexByte("pPdDsSwW", src[t+1]) >= 0 {
			t = skipEscape(src, t) // a class that folds from a table
			continue
		}
		lo, after, readable := classChar(src, t)
		if !readable {
			return 0, 0, false
		}
		t = after
		hi := lo
		if t+1 < len(src) && src[t] == '-' && src[t+1] != ']' {
			if hi, t, readable = classChar(src, t+1); !readable {
				return 0, 0, false
			}
		}
		n += foldSpan(lo, hi)
	}
	return n, t + 1, true
}

// foldSpan is the runes of the range lo-hi that Go's parser folds one at a
// time: none for a range that lies wholly around or outside minFold..maxFold.
func foldSpan(lo, hi rune) int {
	if hi < lo || hi < minFold || lo > maxFold || lo <= minFold && hi >= maxFold {
		return 0
	}
	return int(min(hi, maxFold) - max(lo, minFold) + 1)
}

// skipEscape is where the escape that begins at src[i] ends, for a scan that
// looks for brackets: \p{...} and \x{...} to their brace, any other escape
// after its next rune.
func skipEscape(src string, i int) int {
	if i+2 < len(src) && strings.IndexByte("pPx", src[i+1]) >= 0 && src[i+2] == '{' {
		if end := strings.IndexByte(src[i+2:], '}'); end >= 0 {
			return i + 2 + end + 1
		}
		return len(src)
	}
	if i+1 >= len(src) {
		return len(src)
	}
	_, size := utf8.DecodeRuneInString(src[i+1:])
	return i + 1 + size
}

// classChar reads a rune of a class at src[t], as Go's parseClassChar does.
func classChar(src string, t int) (rune, int, bool) {
	if src[t] == '\\' {
		return escapedRune(src, t)
	}
	r, size := utf8.DecodeRuneInString(src[t:])
	if r == utf8.RuneError && size <= 1 {
		return 0, 0, false
	}
	return r, t + size, true
}

// escapedRune reads the rune of the escape at src[t], as Go's parseEscape
// does: octal, hexadecimal, a C escape, or punctuation escaped.
func escapedRune(src string, t int) (rune, int, bool) {
	if t+1 >= len(src) {
		return 0, 0, false
	}
	c, size := utf8.DecodeRuneInString(src[t+1:])
	rest := t + 1 + size
	switch {
	case c == '0' || '1' <= c && c <= '7' && rest < len(src) && isOctal(src[rest]):
		r := c - '0'
		for k := 1; k < 3 && rest < len(src) && isOctal(src[rest]); k++ {
			r = r*8 + rune(src[rest]-'0')
			rest++
		}
		return r, rest, true
	case c == 'x' && rest < len(src) && src[rest] == '{':
		end := strings.IndexByte(src[rest:], '}')
		if end < 2 {
			return 0, 0, false
		}
		v, err := strconv.ParseUint(src[rest+1:rest+end], 16, 32)
		if err != nil || v > unicode.MaxRune {
			return 0, 0, false
		}
		return rune(v), rest + end + 1, true
	case c == 'x':
		if rest+2 > len(src) {
			return 0, 0, false
		}
		v, err := strconv.ParseUint(src[rest:rest+2], 16, 8)
		if err != nil {
			return 0, 0, false
		}
		return rune(v), rest + 2, true
	}
	if i := strings.IndexRune("afnrtv", c); i >= 0 {
		return rune("\a\f\n\r\t\v"[i]), rest, true
	}
	if c < utf8.RuneSelf && !isAlnum(byte(c)) {
		return c, rest, true
	}
	return 0, 0, false
}

func isOctal(c byte) bool { return '0' <= c && c <= '7' }

func isAlnum(c byte) bool {
	return '0' <= c && c <= '9' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z'
}

// regexSize is the size of the program that Go compiles from a parsed regular
// expression, measured without compiling it, on the expression as Go
// simplifies it before compiling (a repeat x{n,m} becomes n copies of x and
// m-n optional ones), with each character class counted by its ranges, which
// Go builds and compiles one by one. Past maxSchemaRegexSize, it stops
// counting.
func regexSize(re *syntax.Regexp) int {
	return addSize(measureRegex(re, classRanges), 2) // the program's fail and match instructions
}

// regexWeight is the program that Go compiles from a parsed regular
// expression, as matching runs it: each instruction once, a character class
// for a binary search of its ranges. It is never less than the program's
// size in instructions.
func regexWeight(re *syntax.Regexp) int {
	return addSize(measureRegex(re, classSearch), 2)
}

// classRanges is what a character class of n ranges adds to a size.
func classRanges(n int) int { return max(n, 1) }

// classSearch is what a character class of n ranges adds to a weight: one
// instruction, and a step for each 16-fold of its ranges that a binary
// search goes through.
func classSearch(n int) int { return 1 + bits.Len(uint(n))/4 }

func measureRegex(re *syntax.Regexp, class func(int) int) int {
	switch re.Op {
	case syntax.OpLiteral:
		return max(len(re.Rune), 1)
	case syntax.OpCharClass:
		return class(len(re.Rune) / 2)
	case syntax.OpCapture:
		return addSize(measureRegex(re.Sub[0], class), 2)
	case syntax.OpStar:
		return addSize(measureRegex(re.Sub[0], class), 2) // a loop, or (x+)? when x matches the empty string
	case syntax.OpPlus, syntax.OpQuest:
		return addSize(measureRegex(re.Sub[0], class), 1)
	case syntax.OpRepeat:
		sub := measureRegex(re.Sub[0], class)
		switch {
		case re.Max == -1 && re.Min <= 1: // x* or x+
			return addSize(sub, 2)
		case re.Max == -1: // n-1 copies of x, then x+
			return addSize(mulSize(re.Min, sub), 1)
		case re.Max == 0:
			return 1
		}
		return addSize(mulSize(re.Min, sub), mulSize(re.Max-re.Min, addSize(sub, 1)))
	case syntax.OpConcat, syntax.OpAlternate:
		total := len(re.Sub) // the alternation's branches, or the empty concatenation's no-op
		for _, s := range re.Sub {
			total = addSize(total, measureRegex(s, class))
		}
		return total
	}
	return 1 // any character, an empty-width assertion, or an empty or failing match
}

// saturated is past every bound on regular expressions: sizes stop there, so
// that no product overflows.
const saturated = maxSchemaRegexSize + 1

func addSize(a, b int) int { return min(a+b, saturated) }

func mulSize(n, s int) int {
	if n == 0 || s == 0 {
		return 0
	}
	if n > saturated/s {
		return saturated
	}
	return n * s
}

func sizeText(size int) string {
	if size >= saturated {
		return fmt.Sprintf("more than %d", maxSchemaRegexSize)
	}
	return strconv.Itoa(size)
}
