package workloadschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// What one call of Compile may take on the worst schema within the bounds,
// without the race detector: its time and the bytes it allocates, and the
// bytes the compiled schema keeps afterwards.
const (
	compileLimit = 50 * time.Millisecond
	compileAlloc = 16 << 20
	compileKept  = 8 << 20
)

// compileTimeLimit is compileLimit, scaled for the race detector.
func compileTimeLimit() time.Duration {
	if raceDetector {
		return compileLimit * raceSlowdown
	}
	return compileLimit
}

// compiledSchema is what one call of Compile did.
type compiledSchema struct {
	schema *Schema
	err    error
	took   time.Duration
	alloc  uint64 // bytes allocated
	kept   int64  // bytes the compiled schema keeps
}

// compileMeasured compiles a schema and measures the call. No other test runs
// meanwhile: none is parallel.
func compileMeasured(raw string) compiledSchema {
	runtime.GC()
	var before, after, kept runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	s, err := Compile([]byte(raw))
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	runtime.GC()
	runtime.ReadMemStats(&kept)
	c := compiledSchema{
		schema: s, err: err, took: took,
		alloc: after.TotalAlloc - before.TotalAlloc,
		kept:  int64(kept.HeapAlloc) - int64(before.HeapAlloc),
	}
	runtime.KeepAlive(s)
	return c
}

// compileBounded compiles a schema and fails the test unless Compile answers
// within compileTimeLimit and compileAlloc, and the compiled schema keeps at
// most compileKept.
func compileBounded(t *testing.T, name, raw string) compiledSchema {
	t.Helper()
	c := compileMeasured(raw)
	if limit := compileTimeLimit(); c.took > limit {
		t.Errorf("%s: Compile took %v, want at most %v", name, c.took.Round(time.Microsecond), limit)
	}
	if c.alloc > compileAlloc {
		t.Errorf("%s: Compile allocated %d MiB, want at most %d", name, c.alloc>>20, compileAlloc>>20)
	}
	if c.kept > compileKept {
		t.Errorf("%s: the compiled schema keeps %d MiB, want at most %d", name, c.kept>>20, compileKept>>20)
	}
	if c.err != nil && len(c.err.Error()) > maxMessageBytes {
		t.Errorf("%s: an error of %d bytes, want at most %d", name, len(c.err.Error()), maxMessageBytes)
	}
	t.Logf("%s: %d bytes: %v, %d KiB allocated, %d KiB kept: %v", name, len(raw), c.took.Round(time.Microsecond), c.alloc>>10, c.kept>>10, c.err)
	return c
}

// hiddenRanges is the round-5 repro: after the prefix, which opens a class
// with a one-letter Unicode class, as many groups "-B-" and U+1E942 as fit in
// 1 KiB, and the class's end. Go reads the one-letter name as part of the
// escape, then each B-U+1E942 as a range; under case folding it folds each
// such range rune by rune.
func hiddenRanges(prefix string) string {
	group := "-B-" + string(rune(0x1E942))
	var b strings.Builder
	b.WriteString(prefix)
	for b.Len()+len(group)+1 <= maxPatternBytes {
		b.WriteString(group)
	}
	b.WriteString("]")
	return b.String()
}

// k8sLabelKey is the pattern of a Kubernetes label key: an optional DNS
// prefix, then a name.
const k8sLabelKey = `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[a-zA-Z0-9]([-a-zA-Z0-9_.]*[a-zA-Z0-9])?$`

// onePattern is a schema of one pattern.
func onePattern(p string) string {
	b, _ := json.Marshal(p) // a string always encodes
	return `{"pattern":` + string(b) + `}`
}

// manyPatterns is a schema whose members p0, p1, … each have a pattern.
func manyPatterns(ps ...string) string {
	props := make([]string, len(ps))
	for i, p := range ps {
		b, _ := json.Marshal(p)
		props[i] = fmt.Sprintf(`"p%d":{"pattern":%s}`, i, b)
	}
	return `{"properties":{` + strings.Join(props, ",") + `}}`
}

// TestCompileBoundsItsRegularExpressions compiles the regular expressions of
// the findings: character classes that Go's parser and compiler build range
// by range, ranges that it would fold rune by rune under an inline flag, and
// the one-letter Unicode classes of round 5. Each is refused, or compiled,
// within the time and the bytes a call may take. Patterns as schemas really
// write them still compile.
func TestCompileBoundsItsRegularExpressions(t *testing.T) {
	r := strings.Repeat
	pattern := onePattern
	patterns := func(n int, p string) string {
		ps := make([]string, n)
		for i := range ps {
			ps[i] = p
		}
		return manyPatterns(ps...)
	}
	const fold = `\x{42}-\x{1E942}` // a range that Go folds rune by rune, 125 000 of them
	for _, tc := range []struct {
		name     string
		schema   string
		compiles bool
	}{
		{"a class of 16 300 \\pL, in 64 KiB", pattern("[" + r(`\pL`, 16_300) + "]"), false},
		{"a class of 10 000 \\pL", pattern("[" + r(`\pL`, 10_000) + "]"), false},
		{"a case-folded class of 10 000 \\pL", pattern("(?i)[" + r(`\pL`, 10_000) + "]"), false},
		{"a class of 9 000 \\p{Ll}", pattern("[" + r(`\p{Ll}`, 9_000) + "]"), false},
		{"a negated class of 10 000 \\pL", pattern("[^" + r(`\pL`, 10_000) + "]"), false},
		{"9 patterns of 999 \\p{Ll}", patterns(9, r(`\p{Ll}`, 999)), false},
		{"10 patterns of 999 \\pL", patterns(10, r(`\pL`, 999)), false},
		{"18 anchored patterns of 498 \\p{Ll}", patterns(18, "^"+r(`\p{Ll}`, 498)+"$"), false},
		{"19 anchored patterns of 498 \\pL", patterns(19, "^"+r(`\pL`, 498)+"$"), false},
		{"a pattern of 9 300 \\p{Ll}", pattern(r(`\p{Ll}`, 9_300)), false},
		{"a class of 340 \\pL, in 1 KiB", pattern("[" + r(`\pL`, 340) + "]"), false},
		{"340 \\pL, in 1 KiB", pattern(r(`\pL`, 340)), false},
		{"55 case-folded ranges, in 1 KiB", pattern("(?i)[" + r(fold, 55) + "]"), false},
		{"16 patterns of 1 KiB, each with a class of 15 \\pL", patterns(16, "["+r(`\pL`, 15)+"]"+r("x", 1024-47)), false},
		{"16 patterns of 1 KiB, each with a case-folded range", patterns(16, "(?i)["+fold+"]"+r("x", 1024-23)), false},
		{"17 patterns of 1 KiB of letters", patterns(17, r("x", 1024)), false},
		{"a Kubernetes label key", pattern(k8sLabelKey), true},
		{"a DNS label", pattern(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`), true},
		{"letters, digits, _ and -", pattern(`^[\p{L}\p{N}_-]+$`), true},
		{"case-folded hex, an inline flag", pattern(`(?i)^[0-9a-f]+$`), false},
		{"the round-5 repro: \\pN, then 145 ranges, with (?i)", pattern(hiddenRanges(`(?i)[\pN`)), false},
		{"16 such patterns, with (?i)", patterns(16, hiddenRanges(`(?i)[\pN`)), false},
		{"the round-5 repro without (?i)", pattern(hiddenRanges(`[\pN`)), true},
		{"16 such patterns without (?i)", patterns(16, hiddenRanges(`[\pN`)), true},
		{"the repro spelled with \\PN", pattern(hiddenRanges(`[\PN`)), true},
		{"a class of 15 \\pL", pattern("[" + r(`\pL`, 15) + "]"), true},
		{"a class of 16 \\pL", pattern("[" + r(`\pL`, 16) + "]"), false},
		{"17 \\pL outside a class", pattern(r(`\pL`, 17)), false},
		{"a class of 15 \\p{Assigned}", pattern("[" + r(`\p{Assigned}`, 15) + "]"), true},
		{"a class of 16 \\p{Assigned}", pattern("[" + r(`\p{Assigned}`, 16) + "]"), false},
		{"\\P{Assigned}", pattern(`^[\P{Assigned}]+$`), true},
		{"16 patterns of 1 KiB of letters", patterns(16, r("x", 1024)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := compileBounded(t, tc.name, tc.schema)
			if compiled := c.err == nil; compiled != tc.compiles {
				t.Errorf("%s: Compile = %v, want it compiled: %v", tc.name, c.err, tc.compiles)
			}
		})
	}
}

// TestCompileKeepsNoLocationPerSubschema compiles 508 subschemas under one
// member name of 58 000 bytes: the compiled schema does not keep that name
// once per subschema, and an error under it names its location in at most
// 512 bytes.
func TestCompileKeepsNoLocationPerSubschema(t *testing.T) {
	props := make([]string, 508)
	for i := range props {
		props[i] = fmt.Sprintf(`"a%d":true`, i)
	}
	slashes := strings.Repeat("/", 58_000) // each escaped as ~1, twice its length
	c := compileBounded(t, "508 subschemas under a name of 58 000 bytes",
		`{"properties":{"`+slashes+`":{"properties":{`+strings.Join(props, ",")+`}}}}`)
	if c.err != nil {
		t.Fatalf("Compile: %v", c.err)
	}
	short := strings.Repeat("/", 20_000)
	for name, tc := range map[string]struct{ schema, says string }{
		"a keyword under the long name": {`{"properties":{"` + slashes + `":{"type":"bogus"}}}`, `"type" at "#/properties/~1~1`},
		"a $ref under the long name":    {`{"$defs":{"` + slashes + `":{"$ref":"#/$defs/missing"}}}`, `"$ref" at "#/$defs/~1~1`},
		"a cycle at a long location":    {`{"$defs":{"` + short + `":{"$ref":"#/$defs/` + strings.Repeat("~1", 20_000) + `"}}}`, "#/$defs/~1~1"},
	} {
		c := compileBounded(t, name, tc.schema)
		if c.err == nil {
			t.Errorf("%s: Compile succeeded, want an error", name)
			continue
		}
		msg := c.err.Error()
		if !utf8.ValidString(msg) || !strings.Contains(msg, tc.says) {
			t.Errorf("%s: error %.200q…, want valid UTF-8 that says %s", name, msg, tc.says)
		}
	}
}

// TestCompileRefusesInlineFlags refuses every inline flag group, (?flags) and
// (?flags:…), with which Go would fold case or change what . and ^ match:
// JSON Schema's pattern dialect, ECMA-262, has none. Groups that only group
// or name, and text that looks like a flag group inside a class, an escape
// or \Q…\E, are no flag groups.
func TestCompileRefusesInlineFlags(t *testing.T) {
	for _, p := range []string{
		`(?i)abc`, `(?m)^a$`, `(?s)a.b`, `(?U)a+`, `(?-i)a`, `(?i:a)b`, `a(?im)b`, `(?is:x)`, `(?U:a+)`, `(?-m:^a)`,
		`^[a-z]+(?i)[a-z]*$`, `(?:x(?i)y)`,
		// The raw text has (?i: refused wherever it sits, as an accepted over-refusal.
		`^\(?i\)?$`, `[(?i)]`, `\Q(?i)\E`,
	} {
		c := compileBounded(t, p, onePattern(p))
		if c.err == nil || !strings.Contains(c.err.Error(), "inline flag") {
			t.Errorf("Compile(%s) = %v, want the inline flag group refused", p, c.err)
		}
	}
	for _, p := range []string{
		`(?:ab)+`, `(?P<name>a+)`, `(?<name>a+)`, `a\\(?:b)`,
	} {
		if c := compileBounded(t, p, onePattern(p)); c.err != nil {
			t.Errorf("Compile(%s): %v", p, c.err)
		}
	}
}

// TestCompileTakesRealisticPatterns compiles the patterns of the round-5
// re-review: all of them within the bounds, except the ones that use an
// inline flag, which are now refused.
func TestCompileTakesRealisticPatterns(t *testing.T) {
	const (
		ipv6 = `^(([0-9a-fA-F]{1,4}:){7,7}[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,7}:|([0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|` +
			`([0-9a-fA-F]{1,4}:){1,5}(:[0-9a-fA-F]{1,4}){1,2}|([0-9a-fA-F]{1,4}:){1,4}(:[0-9a-fA-F]{1,4}){1,3}|` +
			`([0-9a-fA-F]{1,4}:){1,3}(:[0-9a-fA-F]{1,4}){1,4}|([0-9a-fA-F]{1,4}:){1,2}(:[0-9a-fA-F]{1,4}){1,5}|` +
			`[0-9a-fA-F]{1,4}:((:[0-9a-fA-F]{1,4}){1,6})|:((:[0-9a-fA-F]{1,4}){1,7}|:)|fe80:(:[0-9a-fA-F]{0,4}){0,4}%[0-9a-zA-Z]{1,}|` +
			`::(ffff(:0{1,4}){0,1}:){0,1}((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])|` +
			`([0-9a-fA-F]{1,4}:){1,4}:((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9]))$`
		docker = `^((?:(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])(?:(?:\.(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9]))+)?(?::[0-9]+)?/)?` +
			`[a-z0-9]+(?:(?:(?:[._]|__|[-]*)[a-z0-9]+)*)?(?:/[a-z0-9]+(?:(?:(?:[._]|__|[-]*)[a-z0-9]+)*)?)*)(?::([\w][\w.-]{0,127}))?` +
			`(?:@([A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*[:][[:xdigit:]]{32,}))?$`
	)
	for _, tc := range []struct {
		name     string
		pattern  string
		compiles bool
	}{
		{"k8s label key", k8sLabelKey, true},
		{"k8s label value", `^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`, true},
		{"k8s qualified name", `^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`, true},
		{"DNS-1123 subdomain", `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`, true},
		{"DNS-1123 label", `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`, true},
		{"RFC 1123 hostname", `^(([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])\.)*([A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9\-]{0,61}[A-Za-z0-9])$`, true},
		{"email, simple", `^[^@\s]+@[^@\s]+\.[^@\s]+$`, true},
		{"email, HTML5", "^[a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$", true},
		{"email, (?i)", `(?i)^[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}$`, false},
		{"semver.org", `^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`, true},
		{"semver.org, named", `^(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)(?:-(?P<prerelease>(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+(?P<buildmetadata>[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`, true},
		{"UUID", `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`, true},
		{"UUID, (?i)", `(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`, false},
		{"UUID v4, (?i)", `(?i)^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, false},
		{"letters, digits, _ and -", `^[\p{L}\p{N}_-]+$`, true},
		{"the same, one-letter names", `^[\pL\pN_-]+$`, true},
		{"letters, marks, digits, punctuation", `^[\p{L}\p{M}\p{N}\p{Pc}\p{Pd}\p{Zs}]+$`, true},
		{"identifier, (?i)", `(?i)^[a-z_][a-z0-9_]*$`, false},
		{"hex color, (?i)", `(?i)^#([0-9a-f]{3}|[0-9a-f]{6})$`, false},
		{"Cyrillic, (?i)", `(?i)^[\x{0400}-\x{04FF}]+$`, false},
		{"the BMP, (?i)", `(?i)^[\x{0020}-\x{FFFF}]+$`, false},
		{"IPv4", `^((25[0-5]|(2[0-4]|1\d|[1-9]|)\d)\.?\b){4}$`, true},
		{"IPv6", ipv6, true},
		{"docker reference", docker, true},
		{"memory quantity", `^[0-9]+(Ki|Mi|Gi)?$`, true},
		{"k8s quantity", `^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$`, true},
		{"RFC 3339 date-time", `^\d{4}-\d{2}-\d{2}[Tt ]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$`, true},
		{"cron", `^(@(annually|yearly|monthly|weekly|daily|hourly|reboot))|(@every (\d+(ns|us|µs|ms|s|m|h))+)|((((\d+,)+\d+|(\d+(\/|-)\d+)|\d+|\*) ?){5,7})$`, true},
	} {
		c := compileBounded(t, tc.name, onePattern(tc.pattern))
		switch {
		case tc.compiles && c.err != nil:
			t.Errorf("%s: Compile: %v", tc.name, c.err)
		case !tc.compiles && (c.err == nil || !strings.Contains(c.err.Error(), "inline flag")):
			t.Errorf("%s: Compile = %v, want its inline flag refused", tc.name, c.err)
		}
	}
}

// TestCompileErrorsKeepTheirReasonAndCause holds an error at a long location
// to at most 512 bytes that still say why, and an error cut to 512 bytes to
// the error it wraps.
func TestCompileErrorsKeepTheirReasonAndCause(t *testing.T) {
	for _, n := range []int{300, 460, 600, 30_000} { // twice 30 000 fits a schema
		name := strings.Repeat("k", n)
		for _, tc := range []struct{ what, schema, reason string }{
			{"a type that is none", `{"properties":{"` + name + `":{"type":"bogus"}}}`, "which is not a type"},
			{"a remote $ref", `{"$defs":{"` + name + `":{"$ref":"https://example.com/s.json"}}}`, "remote reference"},
			{"a pointer to nothing", `{"$defs":{"` + name + `":{"$ref":"#/nowhere"}}}`, "points at nothing"},
			{"a cycle", `{"$defs":{"` + name + `":{"$ref":"#/$defs/` + name + `"}}}`, "reference cycle"},
		} {
			_, err := Compile([]byte(tc.schema))
			if err == nil || len(err.Error()) > maxMessageBytes || !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("a name of %d bytes, %s: error of %d bytes %.120q…, want at most %d that says %q",
					n, tc.what, len(fmt.Sprint(err)), fmt.Sprint(err), maxMessageBytes, tc.reason)
			}
		}
	}
	// A regular expression that Go cannot parse: its error quotes all of it,
	// and is cut, and still wraps the parser's error.
	_, err := Compile([]byte(onePattern("(" + strings.Repeat("a", 900))))
	var parse *syntax.Error
	if err == nil || len(err.Error()) > maxMessageBytes || !errors.As(err, &parse) || parse.Code != syntax.ErrMissingParen {
		t.Errorf("Compile of an unclosed group of 900 bytes = %v (%T), want at most %d bytes that wrap a *syntax.Error", err, err, maxMessageBytes)
	}
}

// desyncPattern is a class that Go ends at its first ] after a range whose
// upper end is [, with what follows read at the top level: the reviewer's
// patterns, which a scan that read [: inside a range as a POSIX class would
// skip to :], and miss what hides in between.
func desyncPatterns() map[string]string {
	x := string(rune(0x1E942))
	fill := func(unit string, n int) string { return strings.Repeat(unit, n) }
	return map[string]string{
		"hidden (?i), folded ranges":   `[A-[:](?i)[` + fill("B-"+x, 168) + `]:]]`,
		"hidden (?s)":                  `[A-[:](?s)[` + fill("B-"+x, 168) + `]:]]`,
		"hidden (?m)":                  `[A-[:](?m)[` + fill("B-"+x, 168) + `]:]]`,
		"hidden \\PC":                  `[A-[:][` + fill(`\PC`, 337) + `]:]]`,
		"hidden \\pL":                  `[A-[:][` + fill(`\pL`, 337) + `]:]]`,
		"POSIX class as the lower end": `[A-[:alpha:](?i)[` + fill("B-"+x, 167) + `]:]]`,
	}
}

// TestCompileIsNotDesyncedByARangeEndingInABracket compiles the desync
// patterns: each is refused, or compiled within the time and the bytes a
// call may take. Sixteen of each, a schema of 16 KiB, too.
func TestCompileIsNotDesyncedByARangeEndingInABracket(t *testing.T) {
	for name, p := range desyncPatterns() {
		if len(p) > maxPatternBytes {
			t.Fatalf("%s: the test pattern is %d bytes", name, len(p))
		}
		compileBounded(t, name, onePattern(p))
		ps := make([]string, 16)
		for i := range ps {
			ps[i] = p
		}
		compileBounded(t, name+", sixteen", manyPatterns(ps...))
	}
}

// TestCompileTakesWhatGoParsesAfterARangeEndingInABracket compiles patterns
// that Go parses, with a range whose upper end is [, which a scan that read a
// POSIX class there refused as unreadable.
func TestCompileTakesWhatGoParsesAfterARangeEndingInABracket(t *testing.T) {
	x := string(rune(0x1E942))
	for _, p := range []string{x + `[P-[:^digit:]\x{1E942}`, `[:><-[:alpha:]`} {
		if _, err := regexp.Compile(p); err != nil {
			t.Fatalf("Go does not compile %q: %v", p, err)
		}
		if c := compileBounded(t, p, onePattern(p)); c.err != nil {
			t.Errorf("Compile(%q): %v", p, c.err)
		}
	}
}
