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
		Status:         ss.Status().String(),
		DesiredVersion: ss.DesiredVersion(),
		ActualVersion:  ss.ActualVersion(),
		Attempts:       ss.Attempts(),
		AgentID:        ss.AgentID(),
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
