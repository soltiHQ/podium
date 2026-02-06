package token

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/auth/identity"
)

// Issuer encodes and cryptographically signs an authenticated identity
// into a raw access token string (e.g., JWT).
//
// Implementations are responsible for:
//   - Embedding all required standard and custom claims
//     (issuer, audience, subject, session ID, permissions, etc.).
//   - Setting and validating temporal claims (iat, nbf, exp).
//   - Applying the configured signing algorithm and key material.
//   - Ensuring produced tokens can be verified by a corresponding Verifier.
//
// Issue must return a token that fully represents the provided identity
// and is suitable for transport (e.g., in HTTP Authorization header).
//
// On failure (invalid identity, misconfiguration, signing error),
// Issue must return a non-nil error.
type Issuer interface {
	Issue(ctx context.Context, id *identity.Identity) (string, error)
}
