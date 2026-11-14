package storage_test

import (
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// TestTextQuery checks the one predicate the Query criteria of every backend
// stand for: trimmed, folded as strings.ToLower folds, a substring of any field,
// and literal in `%`, `_` and `\`.
func TestTextQuery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		query  string
		fields []string
		empty  bool
		want   bool
	}{
		{"no query", "", []string{"agent-a"}, true, true},
		{"spaces alone", "  \t ", []string{"agent-a"}, true, true},
		{"no field", "a", nil, false, false},
		{"a substring", "gent", []string{"agent-a"}, false, true},
		{"another case", "AGENT", []string{"agent-a"}, false, true},
		{"the field in another case", "agent", []string{"AGENT-A"}, false, true},
		{"beyond ASCII", "äRZTE", []string{"Ärzte"}, false, true},
		{"spaces around the query", "  agent  ", []string{"agent-a"}, false, true},
		{"spaces inside the query", "a b", []string{"a  b"}, false, false},
		{"any field", "zone1", []string{"id", "name", "https://x.zone1.example.net"}, false, true},
		{"no field holds it", "zone9", []string{"id", "name", "https://x.zone1.example.net"}, false, false},
		{"an underscore is literal", "a_c", []string{"abc"}, false, false},
		{"an underscore matches itself", "a_c", []string{"a_c"}, false, true},
		{"a percent sign is literal", "%", []string{"abc"}, false, false},
		{"a percent sign matches itself", "100%", []string{"tango 100%"}, false, true},
		{"a backslash matches itself", `a\b`, []string{`share a\b`}, false, true},
		{"a trailing backslash matches itself", `a\`, []string{`share a\b`}, false, true},
		{"a backslash is not an escape", `a\b`, []string{"slab"}, false, false},
	} {
		q := storage.NewTextQuery(tc.query)
		if q.Empty() != tc.empty {
			t.Errorf("%s: Empty = %v, want %v", tc.name, q.Empty(), tc.empty)
		}
		if got := q.Matches(tc.fields...); got != tc.want {
			t.Errorf("%s: Matches = %v, want %v", tc.name, got, tc.want)
		}
	}
}
