package storage

import (
	"context"
	"time"
)

// ClockedSessionRotation applies refresh rotation with a caller-owned timestamp.
// Replicated stores and transaction views implement it so FSM replay never
// consults a replica's business clock. A zero updatedAt preserves the stored
// modification time for historical log entries that omitted the timestamp;
// time.Unix(0, 0) is an explicit epoch value, not an omitted timestamp.
// The ordinary Storage.RotateRefresh contract and its callers remain unchanged.
type ClockedSessionRotation interface {
	RotateRefreshAt(ctx context.Context, sessionID string, newHash []byte, newExpiresAt, updatedAt time.Time) error
}
