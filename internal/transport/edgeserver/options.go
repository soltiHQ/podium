package edgeserver

import (
	"time"

	"github.com/rs/zerolog"
)

// Option represents a functional configuration override for the listener.
type Option func(*Config)

// WithHTTPAddr enables the HTTP sync endpoint on the given address.
func WithHTTPAddr(addr string) Option {
	return func(c *Config) {
		c.addrHTTP = addr
	}
}

// WithGRPCAddr enables the gRPC sync endpoint on the given address.
func WithGRPCAddr(addr string) Option {
	return func(c *Config) {
		c.addrGRPC = addr
	}
}

// WithLogLevel overrides the default logging level.
func WithLogLevel(level zerolog.Level) Option {
	return func(c *Config) {
		c.logLevel = level
	}
}

// WithHTTPReadHeaderTimeout sets the maximum time allowed to read HTTP request headers.
func WithHTTPReadHeaderTimeout(d time.Duration) Option {
	return func(c *Config) {
		c.configHTTP.Timeouts.ReadHeader = d
	}
}

// WithHTTPReadTimeout sets the maximum time allowed to read the full HTTP request body.
func WithHTTPReadTimeout(d time.Duration) Option {
	return func(c *Config) {
		c.configHTTP.Timeouts.Read = d
	}
}

// WithHTTPWriteTimeout sets the maximum time allowed to write an HTTP response.
func WithHTTPWriteTimeout(d time.Duration) Option {
	return func(c *Config) {
		c.configHTTP.Timeouts.Write = d
	}
}

// WithHTTPIdleTimeout sets how long to keep idle HTTP connections open before closing them.
func WithHTTPIdleTimeout(d time.Duration) Option {
	return func(c *Config) {
		c.configHTTP.Timeouts.Idle = d
	}
}

// WithGRPCConnectionTimeout sets the maximum time allowed to establish a new gRPC connection.
func WithGRPCConnectionTimeout(d time.Duration) Option {
	return func(c *Config) {
		c.configGRPC.ConnectionTimeout = d
	}
}

// WithGRPCMaxRecvMsgSize sets the maximum size of incoming gRPC messages in bytes.
func WithGRPCMaxRecvMsgSize(size int) Option {
	return func(c *Config) {
		c.configGRPC.Limits.MaxRecvMsgSize = size
	}
}

// WithGRPCMaxSendMsgSize sets the maximum size of outgoing gRPC messages in bytes.
func WithGRPCMaxSendMsgSize(size int) Option {
	return func(c *Config) {
		c.configGRPC.Limits.MaxSendMsgSize = size
	}
}
