package jwt

import (
	"context"
	"errors"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/token"
)

// HSVerifier verifies HMAC-signed JWT access tokens (HS256).
type HSVerifier struct {
	issuer   string
	audience string
	secret   []byte
	clock    token.Clock
}

// NewHSVerifier creates a HS256 JWT verifier.
func NewHSVerifier(issuer, audience string, secret []byte, clock token.Clock) *HSVerifier {
	if clock == nil {
		clock = token.RealClock()
	}
	return &HSVerifier{
		issuer:   issuer,
		audience: audience,
		secret:   append([]byte(nil), secret...),
		clock:    clock,
	}
}

// Verify parses and validates a raw JWT token string.
func (v *HSVerifier) Verify(_ context.Context, rawToken string) (*identity.Identity, error) {
	if rawToken == "" {
		return nil, token.ErrInvalidToken
	}
	if v.issuer == "" || v.audience == "" || len(v.secret) == 0 {
		return nil, token.ErrInvalidToken
	}

	parsed, err := jwtlib.Parse(rawToken, func(t *jwtlib.Token) (any, error) {
		if t.Method == nil || t.Method.Alg() != jwtlib.SigningMethodHS256.Alg() {
			return nil, token.ErrInvalidToken
		}
		return v.secret, nil
	},
		jwtlib.WithValidMethods([]string{jwtlib.SigningMethodHS256.Alg()}),
		jwtlib.WithIssuer(v.issuer),
		jwtlib.WithAudience(v.audience),
		jwtlib.WithTimeFunc(v.clock.Now),
	)
	if err != nil {
		switch {
		case errors.Is(err, jwtlib.ErrTokenExpired),
			errors.Is(err, jwtlib.ErrTokenNotValidYet):
			return nil, token.ErrExpiredToken
		default:
			return nil, token.ErrInvalidToken
		}
	}
	if parsed == nil || !parsed.Valid {
		return nil, token.ErrInvalidToken
	}

	mc, ok := parsed.Claims.(jwtlib.MapClaims)
	if !ok {
		return nil, token.ErrInvalidToken
	}

	id, err := identityFromMapClaims(mc, v.issuer, v.audience)
	if err != nil {
		return nil, err
	}
	id.RawToken = rawToken
	return id, nil
}
