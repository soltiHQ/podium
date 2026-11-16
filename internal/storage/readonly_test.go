package storage_test

import (
	"context"
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// TestMarkReadOnly checks that a context is read-only once marked, and so are
// the contexts derived from it, and no other.
func TestMarkReadOnly(t *testing.T) {
	type otherKey struct{}
	marked := storage.MarkReadOnly(context.Background())
	derived, cancel := context.WithCancel(context.WithValue(marked, otherKey{}, "value"))
	defer cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"an unmarked context", context.Background(), false},
		{"a marked context", marked, true},
		{"a context derived from a marked one", derived, true},
		{"a context with another value", context.WithValue(context.Background(), otherKey{}, true), false},
	} {
		if got := storage.IsReadOnly(tc.ctx); got != tc.want {
			t.Errorf("IsReadOnly of %s = %v, want %v", tc.name, got, tc.want)
		}
	}
}
