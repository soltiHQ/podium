package session

import "time"

// TokenPair is returned to callers after login/refresh.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
}

// Config controls token/session lifetimes and rotation behavior.
type Config struct {
	AccessTTL  time.Duration
	RefreshTTL time.Duration

	Issuer   string
	Audience string

	RotateRefresh bool
}
