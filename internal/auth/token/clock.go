package token

import "time"

// Clock allows deterministic time in tests.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RealClock returns a production clock implementation.
func RealClock() Clock { return realClock{} }
