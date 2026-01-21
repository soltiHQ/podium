package middleware

import (
	"time"

	"github.com/soltiHQ/control-plane/internal/transport/middleware/cors"
)

// HttpChainConfig controls which HTTP middlewares are enabled.
type HttpChainConfig struct {
	// RequestID enables attaching/propagating request ID.
	RequestID bool
	// Recovery enables panic recovery.
	Recovery bool
	// Logging enables access logging.
	Logging bool
	// CORS, if non-nil, enables CORS middleware with provided settings.
	CORS *cors.CORSConfig
}

// GrpcChainConfig controls which gRPC middlewares are enabled.
type GrpcChainConfig struct {
	// RequestID enables attaching/propagating request ID.
	RequestID bool
	// Recovery enables panic recovery.
	Recovery bool
	// Logging enables request logging.
	Logging bool
}

// DefaultHttpChainConfig returns opinionated defaults for an HTTP middleware chain.
func DefaultHttpChainConfig() HttpChainConfig {
	return HttpChainConfig{
		RequestID: true,
		Recovery:  true,
		Logging:   true,
		CORS: &cors.CORSConfig{
			AllowedOrigins:   []string{"*"},
			AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
			AllowedHeaders:   nil,
			ExposedHeaders:   nil,
			MaxAge:           10 * time.Minute,
			AllowCredentials: false,
		},
	}
}

// DefaultGrpcChainConfig returns opinionated defaults for gRPC middleware chain.
func DefaultGrpcChainConfig() GrpcChainConfig {
	return GrpcChainConfig{
		RequestID: true,
		Recovery:  true,
		Logging:   true,
	}
}
