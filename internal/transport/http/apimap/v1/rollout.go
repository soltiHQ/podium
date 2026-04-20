package apimapv1

import (
	"time"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/model"
)

// RolloutSpec maps a domain Spec and its rollouts to the composite DTO.
func RolloutSpec(ts *model.Spec, states []*model.Rollout) restv1.RolloutSpec {
	dto := restv1.RolloutSpec{
		Spec: Spec(ts),
	}
	if ts != nil {
		gen := ts.Generation()
		for _, ss := range states {
			if ss.IsStaleFor(gen) {
				dto.Spec.DirtyForRollout = true
				break
			}
		}
	}
	if len(states) > 0 {
		dto.Entries = make([]restv1.RolloutEntry, 0, len(states))
		for _, ss := range states {
			dto.Entries = append(dto.Entries, RolloutEntry(ss))
		}
	}
	return dto
}

// RolloutEntry maps a domain Rollout to its REST rollout entry DTO.
func RolloutEntry(ss *model.Rollout) restv1.RolloutEntry {
	if ss == nil {
		return restv1.RolloutEntry{}
	}
	dto := restv1.RolloutEntry{
		Status:             ss.Status().String(),
		Intent:             ss.Intent().String(),
		DesiredGeneration:  ss.DesiredGeneration(),
		ObservedGeneration: ss.ObservedGeneration(),
		Attempts:           ss.Attempts(),
		AgentID:            ss.AgentID(),
		ActualTaskID:       ss.ActualTaskID(),
	}
	if !ss.LastPushedAt().IsZero() {
		dto.LastPushedAt = ss.LastPushedAt().Format(time.RFC3339)
	}
	if !ss.LastSyncedAt().IsZero() {
		dto.LastSyncedAt = ss.LastSyncedAt().Format(time.RFC3339)
	}
	if ss.Error() != "" {
		dto.Error = ss.Error()
	}
	return dto
}
