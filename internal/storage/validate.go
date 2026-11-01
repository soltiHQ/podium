package storage

import "github.com/soltiHQ/control-plane/domain"

// ValidateEntity refuses a row no store keeps: nil, or one with no ID or no
// creation time. The in-memory store checks every row it stores with it, and the
// transaction overlay every row it records, so that an operation a transaction
// recorded is one the state machine applies.
func ValidateEntity[T domain.Entity[T]](entity T) error {
	var zero T
	if any(entity) == any(zero) || entity.ID() == "" || entity.CreatedAt().IsZero() {
		return ErrInvalidArgument
	}
	return nil
}
