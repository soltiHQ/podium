package model

import (
	"time"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/kind"
)

var _ domain.Entity[*Rollout] = (*Rollout)(nil)

// Rollout tracks the reconciliation state of a Spec on a specific agent.
//
// The record captures three orthogonal pieces of information:
//
//   - **Intent** ([kind.RolloutIntent]) — what the sync runner should do
//     next: Install, Update, Uninstall, or Noop. Populated by the service
//     layer (Deploy/Delete reconcilers) based on spec-level changes and
//     cleared back to Noop once the agent confirms the action.
//   - **Status** ([kind.SyncStatus]) — what happened last time the sync
//     runner touched this rollout: Pending, Synced, Failed, Drift, or
//     Unknown.
//   - **Generation tracking** — `ObservedGeneration` is the last
//     `Spec.Generation` confirmed on the agent; `DesiredGeneration` is
//     what the rollout should converge to. Differing values are what
//     actually drive a re-create on the next tick.
//
// `ActualTaskID` is the TaskId returned by the agent from the last
// successful SubmitTask. Empty means "the agent has no task for this
// rollout yet". Required for re-create (DeleteTask then SubmitTask) and
// for uninstall paths.
type Rollout struct {
	createdAt    time.Time
	updatedAt    time.Time
	lastPushedAt time.Time
	lastSyncedAt time.Time

	desiredGeneration  int
	observedGeneration int
	attempts           int

	id           string
	specID       string
	agentID      string
	actualTaskID string
	errMsg       string

	status kind.SyncStatus
	intent kind.RolloutIntent
}

// RolloutID returns the deterministic identifier for a Spec-Agent pair.
func RolloutID(specID, agentID string) string {
	return "rid-" + specID + "-" + agentID
}

// NewRollout creates a new Rollout for a Spec-Agent pair with Install
// intent — the default for a freshly-deployed target.
func NewRollout(specID, agentID string, desiredGeneration int) (*Rollout, error) {
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

		desiredGeneration: desiredGeneration,
		status:            kind.SyncStatusPending,
		intent:            kind.RolloutIntentInstall,
	}, nil
}

// --- Getters ---

// ID returns the rollout's unique identifier.
func (ss *Rollout) ID() string { return ss.id }

// SpecID returns the associated Spec ID.
func (ss *Rollout) SpecID() string { return ss.specID }

// AgentID returns the target agent ID.
func (ss *Rollout) AgentID() string { return ss.agentID }

// DesiredGeneration returns the Spec generation the rollout should
// converge to. Set by the Deploy/Delete reconcilers when they queue work.
func (ss *Rollout) DesiredGeneration() int { return ss.desiredGeneration }

// ObservedGeneration returns the Spec generation most recently installed
// on the agent. Compare against `Spec.Generation` (or against
// `DesiredGeneration` — they track the same thing by construction) to
// decide whether a re-create is needed.
func (ss *Rollout) ObservedGeneration() int { return ss.observedGeneration }

// Status returns the current sync status.
func (ss *Rollout) Status() kind.SyncStatus { return ss.status }

// Intent returns what the sync runner should do on the next tick.
func (ss *Rollout) Intent() kind.RolloutIntent { return ss.intent }

// ActualTaskID returns the TaskId the agent reported on the last
// successful SubmitTask, or empty if nothing is installed.
func (ss *Rollout) ActualTaskID() string { return ss.actualTaskID }

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

// --- Setters / transitions ---

// SetIntent records the next reconciliation action the sync runner should
// take. Callers should typically also call [MarkPending] to reset
// attempts/error and signal that the runner should pick this rollout up.
func (ss *Rollout) SetIntent(intent kind.RolloutIntent) {
	ss.intent = intent
	ss.updatedAt = time.Now()
}

// SetActualTaskID records the TaskId the agent returned for this rollout.
// Pass an empty string to clear (used between DeleteTask and SubmitTask
// during an update so a crash mid-flight resumes cleanly).
func (ss *Rollout) SetActualTaskID(taskID string) {
	ss.actualTaskID = taskID
	ss.updatedAt = time.Now()
}

// MarkPending resets the rollout so the sync runner treats it as an
// actionable item on the next tick. `desiredGeneration` is the spec
// generation the rollout must converge to; the intent should already
// be set (Install/Update/Uninstall).
func (ss *Rollout) MarkPending(desiredGeneration int) {
	ss.desiredGeneration = desiredGeneration
	ss.status = kind.SyncStatusPending
	ss.attempts = 0
	ss.errMsg = ""
	ss.updatedAt = time.Now()
}

// MarkSynced marks the agent as having the exact spec generation
// applied. Clears the pending intent: subsequent ticks skip this rollout
// until an external action (edit, redeploy, remove target) moves it out
// of `Noop`.
func (ss *Rollout) MarkSynced(observedGeneration int) {
	ss.observedGeneration = observedGeneration
	ss.status = kind.SyncStatusSynced
	ss.intent = kind.RolloutIntentNoop
	ss.lastSyncedAt = time.Now()
	ss.errMsg = ""
	ss.updatedAt = time.Now()
}

// MarkDrift marks a version mismatch detected via export.
func (ss *Rollout) MarkDrift() {
	ss.status = kind.SyncStatusDrift
	ss.updatedAt = time.Now()
}

// MarkFailed records a push failure. The intent stays as-is so the sync
// runner retries the same action on the next tick (bounded by
// `MaxRetries`).
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

// IsStaleFor reports whether this rollout still has work to do for a
// spec at `generation`: either the sync runner is going to touch the
// agent on the next tick (non-Noop intent), or the agent is running an
// older generation than desired. Used by the REST DTO to surface the
// "apply pending" banner on the spec detail page.
func (ss *Rollout) IsStaleFor(generation int) bool {
	if ss == nil {
		return false
	}
	return ss.intent != kind.RolloutIntentNoop || ss.observedGeneration < generation
}

// Clone creates a deep copy of the Rollout.
func (ss *Rollout) Clone() *Rollout {
	return &Rollout{
		createdAt:    ss.createdAt,
		updatedAt:    ss.updatedAt,
		lastPushedAt: ss.lastPushedAt,
		lastSyncedAt: ss.lastSyncedAt,

		id:           ss.id,
		specID:       ss.specID,
		agentID:      ss.agentID,
		actualTaskID: ss.actualTaskID,
		errMsg:       ss.errMsg,

		desiredGeneration:  ss.desiredGeneration,
		observedGeneration: ss.observedGeneration,
		attempts:           ss.attempts,

		status: ss.status,
		intent: ss.intent,
	}
}
