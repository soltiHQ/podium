package jwt

import (
	"context"
	"time"

	"github.com/soltiHQ/control-plane/auth"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

// Issuer implements auth.Issuer using HMAC-signed JWT tokens (HS256).
type Issuer struct {
	cfg auth.JWTConfig
}

// NewIssuer creates a new JWT issuer with the provided configuration.
func NewIssuer(cfg auth.JWTConfig) *Issuer {
	return &Issuer{cfg: cfg}
}

// Issue signs and returns a JWT token for the given identity.
func (i *Issuer) Issue(_ context.Context, id *auth.Identity) (string, error) {
	now := time.Now()

	claims := jwtlib.MapClaims{
		"iss":   i.cfg.Issuer,
		"aud":   []string{i.cfg.Audience},
		"sub":   id.Subject,
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(i.cfg.TokenTTL).Unix(),
		"jti":   id.TokenID,
		"uid":   id.UserID,
		"perms": id.Permissions,
	}

	t := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, claims)
	return t.SignedString(i.cfg.Secret)
}
