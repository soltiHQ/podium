package dto

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
)

// Status shorthand for the switch inside RolloutFromDTO.
var (
	syncStatusPending = kind.SyncStatusPending
	syncStatusSynced  = kind.SyncStatusSynced
	syncStatusDrift   = kind.SyncStatusDrift
	syncStatusFailed  = kind.SyncStatusFailed
	syncStatusUnknown = kind.SyncStatusUnknown
)

type RolloutDTO struct {
	ID           string
	SpecID       string
	AgentID      string
	ActualTaskID string
	ErrMsg       string

	DesiredGeneration  int
	ObservedGeneration int
	Attempts           int

	Status uint8 // kind.SyncStatus
	Intent uint8 // kind.RolloutIntent

	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastPushedAt time.Time
	LastSyncedAt time.Time
}

func RolloutToDTO(r *model.Rollout) *RolloutDTO {
	return &RolloutDTO{
		ID:                 r.ID(),
		SpecID:             r.SpecID(),
		AgentID:            r.AgentID(),
		ActualTaskID:       r.ActualTaskID(),
		ErrMsg:             r.Error(),
		DesiredGeneration:  r.DesiredGeneration(),
		ObservedGeneration: r.ObservedGeneration(),
		Attempts:           r.Attempts(),
		Status:             uint8(r.Status()),
		Intent:             uint8(r.Intent()),
		CreatedAt:          r.CreatedAt(),
		UpdatedAt:          r.UpdatedAt(),
		LastPushedAt:       r.LastPushedAt(),
		LastSyncedAt:       r.LastSyncedAt(),
	}
}

// RolloutFromDTO reconstructs a Rollout. NewRollout builds the skeleton;
// transition methods (MarkPending/MarkSynced/MarkFailed/MarkDrift) set the
// status+counters to match the serialised state. Errors from transitions
// are ignored here — they indicate programmer-side invariants the stored
// state already violated, which can't be fixed by reporting here.
func RolloutFromDTO(d *RolloutDTO) (*model.Rollout, error) {
	if d == nil {
		return nil, nil
	}
	r, err := model.NewRollout(d.SpecID, d.AgentID, d.DesiredGeneration)
	if err != nil {
		return nil, err
	}
	r.SetIntent(kindRolloutIntent(d.Intent))
	r.SetActualTaskID(d.ActualTaskID)
	switch kindSyncStatus(d.Status) {
	case syncStatusPending:
		r.MarkPending(d.DesiredGeneration)
	case syncStatusSynced:
		r.MarkSynced(d.ObservedGeneration)
	case syncStatusFailed:
		r.MarkFailed(d.ErrMsg)
	case syncStatusDrift:
		r.MarkDrift()
	case syncStatusUnknown:
		r.MarkUnknown()
	}
	r.SetLastPushedAt(d.LastPushedAt)
	r.SetCreatedAt(d.CreatedAt)
	r.SetUpdatedAt(d.UpdatedAt)
	return r, nil
}
