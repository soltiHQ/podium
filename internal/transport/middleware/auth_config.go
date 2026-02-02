package middleware

import authcore "github.com/soltiHQ/control-plane/auth"

// AuthConfig configures authentication middleware/interceptors.
type AuthConfig struct {
	// Verifier validates incoming credentials (e.g. JWT verifier).
	Verifier authcore.Verifier
}
