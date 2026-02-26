package model

import (
	"time"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/kind"
)

var _ domain.Entity[*Rollout] = (*Rollout)(nil)

// Rollout tracks the synchronization state of a Spec on a specific agent.
//
// It records the desired version (what CP wants) vs actual version (what the agent has),
// enabling the sync runner to detect drift and reconcile.
type Rollout struct {
	createdAt time.Time
	updatedAt time.Time

	lastPushedAt time.Time
	lastSyncedAt time.Time

	id      string
	specID  string
	agentID string
	errMsg  string

	desiredVersion int
	actualVersion  int
	attempts       int

	status kind.SyncStatus
}

// RolloutID returns the deterministic identifier for a Spec-Agent pair.
func RolloutID(specID, agentID string) string {
	return "rid-" + specID + "-" + agentID
}

// NewRollout creates a new Rollout for a Spec-Agent pair.
func NewRollout(specID, agentID string, desiredVersion int) (*Rollout, error) {
	if specID == "" || agentID == "" {
		return nil, domain.ErrEmptyID
	}
	now := time.Now()
	return &Rollout{
		createdAt: now,
		updatedAt: now,

		id:      RolloutID(specID, agentID),
		specID:  specID,
		agentID: agentID,

		desiredVersion: desiredVersion,
		status:         kind.SyncStatusPending,
	}, nil
}

// ID returns the rollout's unique identifier.
func (ss *Rollout) ID() string { return ss.id }

// SpecID returns the associated Spec ID.
func (ss *Rollout) SpecID() string { return ss.specID }

// AgentID returns the target agent ID.
func (ss *Rollout) AgentID() string { return ss.agentID }

// DesiredVersion returns the Spec version that should be on the agent.
func (ss *Rollout) DesiredVersion() int { return ss.desiredVersion }

// ActualVersion returns the Spec version confirmed on the agent.
func (ss *Rollout) ActualVersion() int { return ss.actualVersion }

// Status returns the current sync status.
func (ss *Rollout) Status() kind.SyncStatus { return ss.status }

// LastPushedAt returns when the spec was last pushed to the agent.
func (ss *Rollout) LastPushedAt() time.Time { return ss.lastPushedAt }

// LastSyncedAt returns when the agent last confirmed sync.
func (ss *Rollout) LastSyncedAt() time.Time { return ss.lastSyncedAt }

// Error returns the last error message (if any).
func (ss *Rollout) Error() string { return ss.errMsg }

// Attempts returns the retry counter.
func (ss *Rollout) Attempts() int { return ss.attempts }

// CreatedAt returns the creation timestamp.
func (ss *Rollout) CreatedAt() time.Time { return ss.createdAt }

// UpdatedAt returns the last modification timestamp.
func (ss *Rollout) UpdatedAt() time.Time { return ss.updatedAt }

// MarkPending sets the state to pending with a new desired version.
func (ss *Rollout) MarkPending(desiredVersion int) {
	ss.desiredVersion = desiredVersion
	ss.status = kind.SyncStatusPending
	ss.attempts = 0
	ss.errMsg = ""
	ss.updatedAt = time.Now()
}

// MarkSynced marks the agent as having the correct version.
func (ss *Rollout) MarkSynced(actualVersion int) {
	ss.actualVersion = actualVersion
	ss.status = kind.SyncStatusSynced
	ss.lastSyncedAt = time.Now()
	ss.errMsg = ""
	ss.updatedAt = time.Now()
}

// MarkDrift marks a version mismatch detected via export.
func (ss *Rollout) MarkDrift() {
	ss.status = kind.SyncStatusDrift
	ss.updatedAt = time.Now()
}

// MarkFailed records a push failure.
func (ss *Rollout) MarkFailed(errMsg string) {
	ss.status = kind.SyncStatusFailed
	ss.errMsg = errMsg
	ss.attempts++
	ss.lastPushedAt = time.Now()
	ss.updatedAt = time.Now()
}

// MarkUnknown sets the state when the agent is unreachable.
func (ss *Rollout) MarkUnknown() {
	ss.status = kind.SyncStatusUnknown
	ss.updatedAt = time.Now()
}

// SetLastPushedAt records a push attempt timestamp.
func (ss *Rollout) SetLastPushedAt(t time.Time) {
	ss.lastPushedAt = t
	ss.updatedAt = time.Now()
}

// Clone creates a deep copy of the Rollout.
func (ss *Rollout) Clone() *Rollout {
	return &Rollout{
		createdAt:    ss.createdAt,
		updatedAt:    ss.updatedAt,
		lastPushedAt: ss.lastPushedAt,
		lastSyncedAt: ss.lastSyncedAt,

		id:      ss.id,
		specID:  ss.specID,
		agentID: ss.agentID,
		errMsg:  ss.errMsg,

		desiredVersion: ss.desiredVersion,
		actualVersion:  ss.actualVersion,
		attempts:       ss.attempts,

		status: ss.status,
	}
}
