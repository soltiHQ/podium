package domain

import "time"

// Entity defines the contract for mutable domain entities that can be stored in repository-like storage.
//
// Implementations must guarantee:
//   - Stable, unique ID for a lifetime of the entity.
//   - Clone() returns a deep copy (no shared references).
//   - UpdatedAt() changes on every state mutation.
type Entity[T any] interface {
	// ID returns the unique identifier for this entity.
	// Must be non-empty for stored entities.
	ID() string
	// Clone creates a deep copy of the entity.
	Clone() T
	// UpdatedAt returns the last modification timestamp.
	UpdatedAt() time.Time
}
