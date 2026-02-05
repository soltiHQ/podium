package webserver

import (
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/internal/transport/middleware"
)

// Option represents a functional configuration override for the listener.
type Option func(*Config)

// WithHTTPAddr enables the HTTP sync endpoint on the given address.
func WithHTTPAddr(addr string) Option {
	return func(c *Config) {
		c.addrHTTP = addr
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

// WithHTTPMiddlewareConfig sets the HTTP middleware chain configuration.
func WithHTTPMiddlewareConfig(config middleware.HttpChainConfig) Option {
	return func(c *Config) {
		c.configHTTP.Middleware = config
	}
}
