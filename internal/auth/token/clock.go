package token

import "time"

// Clock abstracts time retrieval to make token issuance and verification deterministic and testable.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RealClock returns the default clock implementation backed by time.Now().
func RealClock() Clock { return realClock{} }
