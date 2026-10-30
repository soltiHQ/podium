package event

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

func TestSubjectOfClassifiesEveryKind(t *testing.T) {
	want := map[string]Subject{
		"agent_connected":       SubjectAgent,
		"agent_inactive":        SubjectAgent,
		"agent_disconnected":    SubjectAgent,
		"agent_deleted":         SubjectAgent,
		"spec_created":          SubjectSpec,
		"spec_updated":          SubjectSpec,
		"spec_deployed":         SubjectSpec,
		"sync_failed":           SubjectSpec,
		"user_created":          SubjectUser,
		"user_updated":          SubjectUser,
		"user_deleted":          SubjectUser,
		"user_password_changed": SubjectUser,
		"user_status_changed":   SubjectUser,
		"session_created":       SubjectUser,
		"rate_limited":          SubjectUser,
		// A closed issue can name an agent, a spec or a login subject.
		"issue_closed": SubjectUnknown,
	}
	for _, kind := range declaredKinds(t) {
		if _, ok := want[kind]; !ok {
			t.Errorf("event kind %q has no expected subject in this test; classify it in SubjectOf", kind)
		}
	}
	for kind, subject := range want {
		if got := SubjectOf(kind); got != subject {
			t.Errorf("SubjectOf(%q) = %v, want %v", kind, got, subject)
		}
	}
	for _, kind := range []string{"", "not_a_kind", "Agent_Connected", RefreshAgents} {
		if got := SubjectOf(kind); got != SubjectUnknown {
			t.Errorf("SubjectOf(%q) = %v, want SubjectUnknown", kind, got)
		}
	}
}

// declaredKinds returns the values of the constants in event.go's
// "Event kinds" block.
func declaredKinds(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "event.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST || gen.Doc == nil || !strings.HasPrefix(gen.Doc.Text(), "Event kinds") {
			continue
		}
		for _, spec := range gen.Specs {
			for _, value := range spec.(*ast.ValueSpec).Values {
				lit, ok := value.(*ast.BasicLit)
				if !ok {
					t.Fatalf("event kind %v is not a string literal", value)
				}
				kind, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				kinds = append(kinds, kind)
			}
		}
	}
	if len(kinds) == 0 {
		t.Fatal(`no "Event kinds" constant block in event.go`)
	}
	return kinds
}
