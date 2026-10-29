package model

import (
	"errors"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/domain"
)

// A token over 1024 bytes never becomes a credential: the store copies every
// credential on each write.
func TestNewAgentCredentialBoundsTheToken(t *testing.T) {
	if _, err := NewAgentCredential("agent-1", strings.Repeat("t", 1024)); err != nil {
		t.Fatalf("1024-byte token: %v", err)
	}
	if _, err := NewAgentCredential("agent-1", strings.Repeat("t", 1025)); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("1025-byte token: %v, want ErrInvalidArgument", err)
	}
	if _, err := NewAgentCredential("agent-1", ""); !errors.Is(err, domain.ErrFieldEmpty) {
		t.Fatalf("empty token: %v, want ErrFieldEmpty", err)
	}
}
