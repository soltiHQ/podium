package auth

import "time"

// Config is a high-level authentication configuration.
type Config struct {
	// JWT holds JWT-specific configuration.
	JWT JWTConfig
}

type JWTConfig struct {
	Issuer   string
	Audience string
	Secret   []byte
	TokenTTL time.Duration
}
