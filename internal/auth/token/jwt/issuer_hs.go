package jwt

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/token"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

// HSIssuer issues HMAC-signed JWT access tokens (HS256).
type HSIssuer struct {
	secret []byte
	clock  token.Clock
}

// NewHSIssuer creates a HS256 JWT issuer.
func NewHSIssuer(secret []byte, clock token.Clock) *HSIssuer {
	if clock == nil {
		clock = token.RealClock()
	}
	return &HSIssuer{
		secret: append([]byte(nil), secret...),
		clock:  clock,
	}
}

// Issue signs and returns a JWT token for the given identity.
func (i *HSIssuer) Issue(_ context.Context, id *identity.Identity) (string, error) {
	if id == nil {
		return "", token.ErrInvalidToken
	}

	if id.Issuer == "" || id.Subject == "" || id.UserID == "" {
		return "", token.ErrInvalidToken
	}
	if len(id.Audience) == 0 {
		return "", token.ErrInvalidToken
	}
	if len(i.secret) == 0 {
		return "", token.ErrInvalidToken
	}

	var (
		claims = mapClaimsFromIdentity(id)
		t      = jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, claims)
	)
	return t.SignedString(i.secret)
}
