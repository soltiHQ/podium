package domain

import "time"

// Entity defines the contract for domain models that can be stored.
//
// Implementations must provide:
//   - Unique identification via ID()
//   - Deep cloning to isolate stored state from external mutations via Clone()
//   - Temporal ordering for cursor-based pagination via UpdatedAt()
type Entity[T any] interface {
	// ID returns the unique identifier for this entity.
	// Must be non-empty for stored entities.
	ID() string
	// Clone creates a deep copy of the entity.
	Clone() T
	// UpdatedAt returns the last modification timestamp.
	UpdatedAt() time.Time
}
