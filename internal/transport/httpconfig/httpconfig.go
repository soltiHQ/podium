// Package httpconfig defines reusable HTTP api configuration primitives.
package httpconfig

import (
	"time"
)

const (
	// DefaultReadHeaderTimeout limits how long the api waits for the full HTTP request headers to be read.
	DefaultReadHeaderTimeout = 5 * time.Second
	// DefaultReadTimeout limits how long the api spends reading the entire HTTP request (headers and body).
	DefaultReadTimeout = 15 * time.Second
	// DefaultWriteTimeout limits how long the api spends writing the HTTP response back to the client.
	DefaultWriteTimeout = 30 * time.Second
	// DefaultIdleTimeout controls how long idle keep-alive connections are kept open before being closed.
	DefaultIdleTimeout = 90 * time.Second
)

// Timeouts group per-connection time limits for HTTP servers.
type Timeouts struct {
	// ReadHeader limits the time allowed to read the request headers.
	ReadHeader time.Duration
	// Read limits the time allowed to read the full request (headers + body).
	Read time.Duration
	// Write limits the time allowed to write the response.
	Write time.Duration
	// Idle limits how long idle keep-alive connections are kept open.
	Idle time.Duration
}

// Config holds common HTTP-related configuration shared by different transport surfaces.
type Config struct {
	Timeouts Timeouts
}

// New returns a Config initialized with the package defaults.
func New() Config {
	return Config{
		Timeouts: Timeouts{
			ReadHeader: DefaultReadHeaderTimeout,
			Read:       DefaultReadTimeout,
			Write:      DefaultWriteTimeout,
			Idle:       DefaultIdleTimeout,
		},
	}
}
