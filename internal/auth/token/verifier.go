package token

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/auth/identity"
)

// Verifier validates a raw access token string and returns the authenticated identity.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (*identity.Identity, error)
}
