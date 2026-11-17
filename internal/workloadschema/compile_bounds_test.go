package workloadschema

import (
	"encoding/json"
	"fmt"
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

// k8sLabelKey is the pattern of a Kubernetes label key: an optional DNS
// prefix, then a name.
const k8sLabelKey = `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[a-zA-Z0-9]([-a-zA-Z0-9_.]*[a-zA-Z0-9])?$`

// TestCompileBoundsItsRegularExpressions compiles the regular expressions of
// the round-3 findings, whose character classes Go's parser and compiler
// build range by range, and the ones whose ranges Go folds rune by rune: each
// is refused, or compiled, within the time and the bytes a call may take.
// Patterns as schemas really write them still compile.
func TestCompileBoundsItsRegularExpressions(t *testing.T) {
	r := strings.Repeat
	quoted := func(p string) string {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	pattern := func(p string) string { return `{"pattern":` + quoted(p) + `}` }
	patterns := func(n int, p string) string {
		props := make([]string, n)
		for i := range props {
			props[i] = fmt.Sprintf(`"p%d":{"pattern":%s}`, i, quoted(p))
		}
		return `{"properties":{` + strings.Join(props, ",") + `}}`
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
		{"case-folded hex", pattern(`(?i)^[0-9a-f]+$`), true},
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
