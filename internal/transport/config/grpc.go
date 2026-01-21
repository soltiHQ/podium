package config

import (
	"time"

	"github.com/soltiHQ/control-plane/internal/transport/middleware"
)

const (
	// DefaultMaxRecvMsgSize limits the maximum size of incoming gRPC messages in bytes.
	DefaultMaxRecvMsgSize = 4 << 20 // 4 MiB
	// DefaultMaxSendMsgSize limits the maximum size of outgoing gRPC messages in bytes.
	DefaultMaxSendMsgSize = 4 << 20 // 4 MiB
	// DefaultConnectionTimeout limits how long we wait for a new connection / handshake
	DefaultConnectionTimeout = 5 * time.Second
)

// Limits groups message size limits for gRPC servers.
type Limits struct {
	// MaxRecvMsgSize is the maximum size of incoming messages in bytes.
	MaxRecvMsgSize int
	// MaxSendMsgSize is the maximum size of outgoing messages in bytes.
	MaxSendMsgSize int
}

// GrpcConfig holds a common gRPC-related configuration shared by different transport surfaces.
type GrpcConfig struct {
	Limits     Limits
	Middleware middleware.GrpcChainConfig
	// ConnectionTimeout is how long we wait for a new connection / handshake.
	ConnectionTimeout time.Duration
}

// NewGrpcConfig returns a Config initialized with package defaults.
func NewGrpcConfig() GrpcConfig {
	return GrpcConfig{
		Limits: Limits{
			MaxRecvMsgSize: DefaultMaxRecvMsgSize,
			MaxSendMsgSize: DefaultMaxSendMsgSize,
		},
		Middleware:        middleware.DefaultGrpcChainConfig(),
		ConnectionTimeout: DefaultConnectionTimeout,
	}
}
