package jwt

import (
	"context"
	"errors"
	"time"

	"github.com/soltiHQ/control-plane/auth"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

// Verifier implements auth.Verifier for HMAC-signed JWT tokens (HS256).
type Verifier struct {
	cfg auth.JWTConfig
}

// NewVerifier creates a new JWT verifier.
func NewVerifier(cfg auth.JWTConfig) *Verifier {
	return &Verifier{cfg: cfg}
}

// Verify parses and validates a raw JWT token string.
func (v *Verifier) Verify(_ context.Context, rawToken string) (*auth.Identity, error) {
	if rawToken == "" {
		return nil, auth.ErrInvalidToken
	}

	tok, err := jwtlib.Parse(rawToken, func(t *jwtlib.Token) (any, error) {
		if t.Method != jwtlib.SigningMethodHS256 {
			return nil, auth.ErrInvalidToken
		}
		return v.cfg.Secret, nil
	},
		jwtlib.WithAudience(v.cfg.Audience),
		jwtlib.WithIssuer(v.cfg.Issuer),
	)
	if err != nil {
		if errors.Is(err, jwtlib.ErrTokenExpired) {
			return nil, auth.ErrExpiredToken
		}
		return nil, auth.ErrInvalidToken
	}
	if !tok.Valid {
		return nil, auth.ErrInvalidToken
	}
	mc, ok := tok.Claims.(jwtlib.MapClaims)
	if !ok {
		return nil, auth.ErrInvalidToken
	}

	id := &auth.Identity{
		RawToken: rawToken,
		Issuer:   v.cfg.Issuer,
	}
	if sub, _ := mc["sub"].(string); sub != "" {
		id.Subject = sub
	}
	if uid, _ := mc["uid"].(string); uid != "" {
		id.UserID = uid
	}
	if perms, ok := mc["perms"].([]any); ok {
		id.Permissions = make([]string, 0, len(perms))
		for _, p := range perms {
			if s, ok := p.(string); ok {
				id.Permissions = append(id.Permissions, s)
			}
		}
	}

	id.IssuedAt = time.Unix(int64FromClaim(mc["iat"]), 0)
	id.NotBefore = time.Unix(int64FromClaim(mc["nbf"]), 0)
	id.ExpiresAt = time.Unix(int64FromClaim(mc["exp"]), 0)
	return id, nil
}

func int64FromClaim(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	default:
		return 0
	}
}
