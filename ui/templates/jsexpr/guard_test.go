package jsexpr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// verb is a format verb, with optional flags, width and argument index, as
// in %s, %-5v or %[1]s.
const verb = `%[-+# 0-9.]*(?:\[\d+\])?[a-zA-Z]`

// bq is a backquote, the delimiter of a Go raw string.
const bq = "`"

// quotedVerb is a format verb right after a JavaScript quote, as in
// fetch('%s') or remove("%[1]v"): the value lands inside a hand-written string
// literal. It applies to the text of a Go string literal.
var quotedVerb = regexp.MustCompile(`['"]` + verb)

// templShapes finds the same mistakes in a line of a .templ file, where a
// JavaScript double quote is \" inside a Go string and " inside a raw string:
// a quoted format verb, or a + that joins a value to a Go string or raw string
// ending or starting with a JavaScript quote, as in "fetch('" + url + "')" or
// `remove("` + id + `")`. Raw strings are also checked with quotedVerb.
var templShapes = regexp.MustCompile(strings.Join([]string{
	`(?:'|\\")` + verb,                         // '%s, \"%[1]s
	`(?:'["` + bq + `]|\\""|"` + bq + `)\s*\+`, // '" +, '` +, \"" +, "` +
	`\+\s*(?:["` + bq + `]'|"\\"|` + bq + `")`, // + "', + `', + "\", + `"
}, "|"))

// Every dynamic value in an Alpine or HTMX expression goes through String. The
// templates and their Go builders must not paste a value between JavaScript
// quotes instead, with a quoted format verb or with a + at a quote.
func TestNoValuePastedBetweenQuotes(t *testing.T) {
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir(), strings.HasSuffix(path, "_test.go"), strings.HasSuffix(path, "_templ.go"):
			return nil
		case strings.HasSuffix(path, ".templ"):
			checkTempl(t, path)
		case strings.HasSuffix(path, ".go"):
			checkGo(t, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// checkGo reports string literals of a Go file that hold a quoted format verb,
// or that end or start with a quote where + joins them to a value.
func checkGo(t *testing.T, path string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if s, ok := stringLit(n); ok && quotedVerb.MatchString(s) {
				t.Errorf("%s: quoted format verb in %s: encode the value with jsexpr.String", fset.Position(n.Pos()), n.Value)
			}
		case *ast.BinaryExpr:
			if n.Op != token.ADD {
				break
			}
			left, leftLit := stringLit(n.X)
			right, rightLit := stringLit(n.Y)
			endsQuoted := leftLit && !rightLit && (strings.HasSuffix(left, "'") || strings.HasSuffix(left, `"`))
			startsQuoted := rightLit && !leftLit && (strings.HasPrefix(right, "'") || strings.HasPrefix(right, `"`))
			if endsQuoted || startsQuoted {
				t.Errorf("%s: a value joined at a quote: encode it with jsexpr.String", fset.Position(n.Pos()))
			}
		}
		return true
	})
}

// checkTempl reports the lines of a .templ file that paste a value between
// quotes. Comment lines are skipped.
func checkTempl(t *testing.T, path string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		raw := strings.Split(line, bq)
		inRaw := false
		for j := 1; j < len(raw); j += 2 {
			inRaw = inRaw || quotedVerb.MatchString(raw[j])
		}
		if inRaw || templShapes.MatchString(line) {
			t.Errorf("%s:%d: a value pasted between quotes: encode it with jsexpr.String:\n%s", path, i+1, line)
		}
	}
}

// stringLit returns the value of a string literal.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
