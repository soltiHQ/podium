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
		"agent_connected":        SubjectAgent,
		"agent_inactive":         SubjectAgent,
		"agent_disconnected":     SubjectAgent,
		"agent_deleted":          SubjectAgent,
		"agent_credential_reset": SubjectAgent,
		"spec_created":           SubjectSpec,
		"spec_updated":           SubjectSpec,
		"spec_deployed":          SubjectSpec,
		"sync_failed":            SubjectSpec,
		"user_created":           SubjectUser,
		"user_updated":           SubjectUser,
		"user_deleted":           SubjectUser,
		"user_password_changed":  SubjectUser,
		"user_status_changed":    SubjectUser,
		"session_created":        SubjectUser,
		"rate_limited":           SubjectUser,
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
	return declaredConsts(t, `the "Event kinds" block`, func(gen *ast.GenDecl, _ string) bool {
		return gen.Doc != nil && strings.HasPrefix(gen.Doc.Text(), "Event kinds")
	})
}

// declaredRefreshKinds returns the values of the constants in event.go whose
// names start with Refresh, wherever in the file they are declared.
func declaredRefreshKinds(t *testing.T) []string {
	t.Helper()
	return declaredConsts(t, "the Refresh constants", func(_ *ast.GenDecl, name string) bool {
		return strings.HasPrefix(name, "Refresh")
	})
}

// declaredConsts returns the values of the string constants of event.go that
// pick selects, by the declaration they are in and their name. what says what is
// picked, for the failure when nothing is.
func declaredConsts(t *testing.T, what string, pick func(gen *ast.GenDecl, name string) bool) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "event.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			spec := spec.(*ast.ValueSpec)
			for i, name := range spec.Names {
				if !pick(gen, name.Name) {
					continue
				}
				if i >= len(spec.Values) {
					t.Fatalf("event kind %s has no value of its own", name.Name)
				}
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("event kind %s is not a string literal", name.Name)
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
		t.Fatalf("no constants in event.go for %s", what)
	}
	return kinds
}

// Every refresh trigger is a refresh kind, so that Notify coalesces it; no
// activity kind and no other string is.
func TestIsRefreshKind(t *testing.T) {
	for _, kind := range declaredRefreshKinds(t) {
		if !IsRefreshKind(kind) {
			t.Errorf("IsRefreshKind(%q) = false: add the trigger to refreshKinds", kind)
		}
	}
	others := append(declaredKinds(t), "", "not_a_kind", "Agent_Update", "agent_update ")
	for _, kind := range others {
		if IsRefreshKind(kind) {
			t.Errorf("IsRefreshKind(%q) = true, want false", kind)
		}
	}
}
