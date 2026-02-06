package token

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/auth/identity"
)

// Verifier validates a raw access token and reconstructs
// the authenticated principal represented by that token.
//
// Implementations are responsible for:
//   - Verifying token signature and allowed algorithms.
//   - Validating issuer and audience.
//   - Enforcing temporal claims (exp, nbf, iat) using a trusted clock.
//   - Ensuring required custom claims (e.g., user ID, session ID) are present.
//
// On success, Verify returns a fully populated identity.Identity
// suitable for authorization checks and placement into request context.
//
// On failure, implementations must return a well-defined error
// (e.g., ErrInvalidToken, ErrExpiredToken) without leaking sensitive details.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (*identity.Identity, error)
}
