package auth

import "context"

// Issuer signs and encodes an Identity into a raw access token string.
//
// Implementations are responsible for:
//   - Embedding all required authorization claims (e.g., permissions, subject, user ID).
//   - Setting temporal claims (iat, nbf, exp).
//   - Applying the configured signing algorithm and secret.
//
// The returned token must be verifiable by a corresponding Verifier implementation.
type Issuer interface {
	// Issue generates and signs a token for the given identity.
	Issue(ctx context.Context, id *Identity) (string, error)
}
