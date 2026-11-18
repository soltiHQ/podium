package workloadschema

import (
	"errors"
	"fmt"
	"math/bits"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A regular expression of a schema, a pattern or a patternProperties key, is
// bounded before Go parses it, and again before Go compiles it, so that
// neither Go's parser nor its compiler does more than the bounds allow:
//
//   - by its source: at most maxPatternBytes, and maxSchemaPatternBytes for
//     all those of a schema;
//   - by its syntax, read as Go's parser reads it (scanPattern): no inline
//     flag group, so that Go never folds case, which would build each range of
//     a class rune by rune; and at most maxUnicodeClasses Unicode classes
//     (\p, \P), each of which Go builds from a table of up to
//     unicodeClassWork ranges, and maxSchemaPatternWork such ranges for all;
//   - by the size of its program, measured on the parsed expression
//     (regexSize), with each character class counted by its ranges: at most
//     maxRegexSize, and maxSchemaRegexSize for all.
//
// Matching it is charged by its weight (regexWeight), the program as matching
// runs it, which a class enters by a binary search of its ranges.
const (
	maxPatternBytes       = 1 << 10
	maxSchemaPatternBytes = 16 << 10
	maxUnicodeClasses     = 15
	maxSchemaPatternWork  = 128_000
	maxRegexSize          = 2_000
	maxSchemaRegexSize    = 20_000
)

// unicodeClassWork is the most ranges that Go's parser appends for one Unicode
// class escape: those of the largest table a name it accepts resolves to, or
// of its negation (\P{C} in Go 1.27). TestUnicodeClassWorkIsTheLargestClass
// computes it from the tables.
const unicodeClassWork = 833

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
	classes, err := scanPattern(text)
	if err != nil {
		return nil, err
	}
	if classes > maxUnicodeClasses {
		return nil, fmt.Errorf("is a regular expression of %d Unicode classes, over the limit of %d", classes, maxUnicodeClasses)
	}
	if b.work += classes * unicodeClassWork; b.work > maxSchemaPatternWork {
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

// errUnreadable is the refusal of a pattern that scanPattern cannot read as
// Go's parser does: one that the parser refuses too.
var errUnreadable = errors.New("is a regular expression that Compile cannot read")

// scanPattern bounds what a pattern makes Go's parser build, and counts its
// Unicode class escapes (\p, \P, with a name in braces or of one letter), as
// the larger of two counts: that of the structural scan (scanStructure), which
// reads the pattern as Go's parser does, and that of the raw text, which no
// misreading can desync. It refuses an inline flag group, (? followed by a
// flag (i, m, s, U) or by -, as (?flags) and (?flags:...) begin: JSON Schema's
// patterns, ECMA-262, have none. That is a search of the raw text too, so it
// refuses such text inside a class, after a backslash and inside \Q...\E as
// well, which Go reads as no flag group: it fails closed. Groups that only
// group or name, (?:, (?P<name> and (?<name>, are kept. It refuses a pattern
// it cannot read, which Go's parser refuses as well.
func scanPattern(src string) (int, error) {
	// The raw text, which no misreading of the syntax can hide from: any
	// (? followed by a flag, wherever it sits, even where Go reads a class
	// (fail closed); and every \p and \P, an upper bound on the Unicode
	// classes (an escaped backslash before p counts too).
	for i := 0; i+2 < len(src); i++ {
		if strings.HasPrefix(src[i:], "(?") && strings.IndexByte("imsU-", src[i+2]) >= 0 {
			return 0, fmt.Errorf("is a regular expression with an inline flag group, %s: JSON Schema's patterns have no inline flags", quote(src[i:i+3]))
		}
	}
	raw := strings.Count(src, `\p`) + strings.Count(src, `\P`)
	classes, err := scanStructure(src)
	if err != nil {
		return 0, err
	}
	return max(classes, raw), nil
}

// scanStructure is the structural scan of scanPattern: the Unicode classes
// that the syntax, as Go reads it, holds.
func scanStructure(src string) (int, error) {
	classes := 0
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], `\Q`): // literal text up to \E, or to the end
			end := strings.Index(src[i+2:], `\E`)
			if end < 0 {
				return classes, nil
			}
			i += 2 + end + 2
		case src[i] == '\\':
			end, class, ok := escapeEnd(src, i)
			if !ok {
				return 0, errUnreadable
			}
			if class {
				classes++
			}
			i = end
		case src[i] == '[':
			end, n, ok := classEnd(src, i)
			if !ok {
				return 0, errUnreadable
			}
			classes += n
			i = end
		default:
			i++
		}
	}
	return classes, nil
}

// escapeEnd is where the escape that begins at src[i] ends, as Go's parser
// reads it, and whether it is a Unicode class: \p or \P with a name in braces
// or of one rune. \x takes braces or two bytes, and an octal escape up to two
// more digits; any other escape is the backslash and one rune. It reports
// false for an escape it cannot read: a trailing backslash, a \p without its
// name, a brace without its end.
func escapeEnd(src string, i int) (int, bool, bool) {
	if i+1 >= len(src) {
		return 0, false, false
	}
	switch c := src[i+1]; {
	case c == 'p' || c == 'P' || c == 'x':
		if i+2 >= len(src) {
			return 0, false, false
		}
		if src[i+2] == '{' {
			end := strings.IndexByte(src[i+2:], '}')
			if end < 0 {
				return 0, false, false
			}
			return i + 2 + end + 1, c != 'x', true
		}
		if c == 'x' {
			return min(i+4, len(src)), false, true
		}
		_, size := utf8.DecodeRuneInString(src[i+2:])
		return i + 2 + size, true, true
	case '0' <= c && c <= '7':
		end := i + 2
		for k := 0; k < 2 && end < len(src) && '0' <= src[end] && src[end] <= '7'; k++ {
			end++
		}
		return end, false, true
	}
	_, size := utf8.DecodeRuneInString(src[i+1:])
	return i + 1 + size, false, true
}

// classEnd is where the class that begins at src[i] ends, as Go's parseClass
// reads it, and the Unicode classes it holds. Its first ], after an optional
// ^, is a literal. An item is read as Go reads one: [:name:] (a POSIX class)
// and \p, \P (a Unicode class) only at its start; otherwise a class
// character, an escape or one rune, with an optional range to a second class
// character, whose upper end is never a POSIX class or a Unicode class: there
// [ is a plain character, and \p is an escape that Go refuses. It reports
// false for a class it cannot read, or without its end.
func classEnd(src string, i int) (int, int, bool) {
	classes, t := 0, i+1
	if t < len(src) && src[t] == '^' {
		t++
	}
	for first := true; t < len(src) && (src[t] != ']' || first); first = false {
		if strings.HasPrefix(src[t:], "[:") {
			if end := strings.Index(src[t+2:], ":]"); end >= 0 {
				t += 2 + end + 2
				continue
			}
		}
		end, class, ok := classChar(src, t)
		if !ok {
			return 0, 0, false
		}
		if class {
			classes++
			t = end
			continue
		}
		t = end
		if t+1 < len(src) && src[t] == '-' && src[t+1] != ']' { // a range: lo-hi
			end, class, ok = classChar(src, t+1)
			if !ok || class {
				return 0, 0, false
			}
			t = end
		}
	}
	if t >= len(src) {
		return 0, 0, false
	}
	return t + 1, classes, true
}

// classChar is where the class character at src[t] ends: an escape, as
// escapeEnd reads it, or one rune, and whether it is a Unicode class.
func classChar(src string, t int) (int, bool, bool) {
	if src[t] == '\\' {
		return escapeEnd(src, t)
	}
	_, size := utf8.DecodeRuneInString(src[t:])
	return t + size, false, true
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
