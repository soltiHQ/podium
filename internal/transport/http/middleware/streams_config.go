package middleware

import "errors"

// ErrNegativeSubscriberBuffer indicates a negative subscriber buffer; zero uses
// the package default.
var ErrNegativeSubscriberBuffer = errors.New("middleware: negative streams.subscriber_buffer")

// StreamsConfig parametrises streaming-endpoint protections.
type StreamsConfig struct {
	// MaxPerIP caps concurrent streams from a single source IP.
	// 0 or negative disables the cap.
	MaxPerIP int `yaml:"max_per_ip"`

	// SubscriberBuffer is the per-subscriber channel buffer in the log-fanout hub.
	// Larger = more tolerance for slow readers before a Lagged is emitted.
	// 0 uses the package default (64).
	SubscriberBuffer int `yaml:"subscriber_buffer"`
}

// Validate reports a negative subscriber buffer. MaxPerIP takes any value: 0 or
// negative disables the cap.
func (c StreamsConfig) Validate() error {
	if c.SubscriberBuffer < 0 {
		return ErrNegativeSubscriberBuffer
	}
	return nil
}
