package transportctx

import (
	"strings"

	"github.com/segmentio/ksuid"
)

const DefaultRequestIDHeader = "x-request-id"

// NewRequestID generates a new request id.
func NewRequestID() string {
	return ksuid.New().String()
}

// NormalizeRequestID trims and validates a request id.
// Returns empty string if it should be treated as missing.
func NormalizeRequestID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 128 {
		return ""
	}
	return s
}
