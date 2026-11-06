package model

import (
	"bytes"
	"errors"
	"testing"

	"github.com/soltiHQ/control-plane/domain"
)

// A credential is one of two kinds. A derived one holds its generation and the
// ID of the enrollment secret it derives from, and no token; a stored one holds
// the raw token of an agent that enrolled without the secret. Each needs its
// agent ID, and what its kind holds.
func TestAgentCredentialKinds(t *testing.T) {
	secretID := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	derived, err := NewDerivedAgentCredential("agent-1", 3, secretID)
	if err != nil {
		t.Fatal(err)
	}
	if !derived.Derived() || derived.Generation() != 3 || !bytes.Equal(derived.SecretID(), secretID) || derived.Token() != "" || derived.ID() != "agent-1" {
		t.Fatalf("derived credential: derived %v, generation %d, secret ID %x, %d-byte token; want derived at 3 under %x, no token",
			derived.Derived(), derived.Generation(), derived.SecretID(), len(derived.Token()), secretID)
	}
	stored, err := NewStoredAgentCredential("agent-2", "raw-token")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Derived() || stored.Generation() != 0 || stored.SecretID() != nil || stored.Token() != "raw-token" {
		t.Fatalf("stored credential: derived %v, generation %d, secret ID %x, token %q; want the token alone",
			stored.Derived(), stored.Generation(), stored.SecretID(), stored.Token())
	}

	for _, tc := range []struct {
		name  string
		build func() (*AgentCredential, error)
		want  error
	}{
		{"derived without an agent ID", func() (*AgentCredential, error) { return NewDerivedAgentCredential("", 0, secretID) }, domain.ErrEmptyID},
		{"derived without a secret ID", func() (*AgentCredential, error) { return NewDerivedAgentCredential("agent-1", 0, nil) }, domain.ErrFieldEmpty},
		{"stored without an agent ID", func() (*AgentCredential, error) { return NewStoredAgentCredential("", "raw-token") }, domain.ErrEmptyID},
		{"stored without a token", func() (*AgentCredential, error) { return NewStoredAgentCredential("agent-2", "") }, domain.ErrFieldEmpty},
	} {
		if _, err := tc.build(); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

// The secret ID a credential keeps is its own: changing the slice it was built
// from, the one SecretID returns or that of a clone leaves it as it was.
func TestAgentCredentialKeepsItsSecretID(t *testing.T) {
	secretID := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	c, err := NewDerivedAgentCredential("agent-1", 0, secretID)
	if err != nil {
		t.Fatal(err)
	}
	secretID[0] = 0xff
	c.SecretID()[1] = 0xff
	c.Clone().secretID[2] = 0xff
	if want := []byte{1, 2, 3, 4, 5, 6, 7, 8}; !bytes.Equal(c.SecretID(), want) {
		t.Fatalf("secret ID %x, want %x", c.SecretID(), want)
	}
}
