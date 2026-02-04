package auth

import (
	"context"
)

// Verifier validates a raw token string and returns an authenticated Identity.
//
// Implementations must:
//   - Verify signature and algorithm.
//   - Validate issuer and audience.
//   - Validate temporal claims (exp, nbf).
//   - Populate Identity with all embedded authorization data.
//
// Returned errors must use:
//   - ErrInvalidToken for malformed or unverifiable tokens.
//   - ErrExpiredToken for expired tokens.
//   - ErrUnauthorized for structurally valid but unauthorized tokens.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (*Identity, error)
}
