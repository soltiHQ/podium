package wire

import (
	"encoding/json"
	"time"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/proxy"
)

// AgentToREST maps a domain Agent to its REST DTO.
func AgentToREST(a *model.Agent) restv1.Agent {
	if a == nil {
		return restv1.Agent{}
	}
	return restv1.Agent{
		ID:   a.ID(),
		Name: a.Name(),

		OS:            a.OS(),
		Arch:          a.Arch(),
		Platform:      a.Platform(),
		UptimeSeconds: a.UptimeSeconds(),

		Metadata: a.MetadataAll(),
		Labels:   a.LabelsAll(),

		Endpoint:     a.Endpoint(),
		EndpointType: string(a.EndpointType()),
		APIVersion:   a.APIVersion().String(),

		Status:            a.Status().String(),
		LastSeenAt:        a.LastSeenAt().Format(time.RFC3339),
		HeartbeatInterval: int(a.HeartbeatInterval().Seconds()),
	}
}

// UserToREST maps a domain User to its REST DTO. Role-ID → Role-name lookup
// uses the built-in role catalogue (kind.BuiltinRoles); unknown IDs fall
// through as themselves.
func UserToREST(u *model.User) restv1.User {
	if u == nil {
		return restv1.User{}
	}
	perms := u.PermissionsAll()
	permStr := make([]string, 0, len(perms))
	for _, p := range perms {
		permStr = append(permStr, string(p))
	}
	roleIDs := u.RoleIDsAll()
	roleNames := make([]string, 0, len(roleIDs))
	for _, id := range roleIDs {
		if name, ok := roleNameByID[id]; ok {
			roleNames = append(roleNames, name)
		} else {
			roleNames = append(roleNames, id)
		}
	}
	return restv1.User{
		RoleIDs:     roleIDs,
		RoleNames:   roleNames,
		Disabled:    u.Disabled(),
		Subject:     u.Subject(),
		Email:       u.Email(),
		Name:        u.Name(),
		ID:          u.ID(),
		Permissions: permStr,
	}
}

// RoleToREST maps a domain Role to its REST DTO.
func RoleToREST(r *model.Role) restv1.Role {
	if r == nil {
		return restv1.Role{}
	}
	return restv1.Role{
		ID:   r.ID(),
		Name: r.Name(),
	}
}

// PermissionToREST returns the string form of a domain permission.
func PermissionToREST(p kind.Permission) string {
	return string(p)
}

// SessionToREST maps a domain Session to its REST DTO.
func SessionToREST(s *model.Session) restv1.Session {
	if s == nil {
		return restv1.Session{}
	}
	return restv1.Session{
		CreatedAt: s.CreatedAt(),
		UpdatedAt: s.UpdatedAt(),
		ExpiresAt: s.ExpiresAt(),
		RevokedAt: s.RevokedAt(),

		ID:           s.ID(),
		UserID:       s.UserID(),
		CredentialID: s.CredentialID(),
		AuthKind:     string(s.AuthKind()),

		Revoked: s.Revoked(),
	}
}

// SpecToREST maps a domain Spec to its REST DTO. Attaches the canonical
// proto-JSON preview via proxy.SpecToProto — used by the UI to show what
// the agent will actually receive.
func SpecToREST(ts *model.Spec) restv1.Spec {
	if ts == nil {
		return restv1.Spec{}
	}
	out := restv1.Spec{
		ID:                ts.ID(),
		Name:              ts.Name(),
		Slot:              ts.Slot(),
		Version:           ts.Version(),
		Generation:        ts.Generation(),
		DeletionRequested: ts.DeletionRequested(),

		KindType:   string(ts.KindType()),
		KindConfig: ts.KindConfig(),

		TimeoutMs:      ts.TimeoutMs(),
		IntervalMs:     ts.IntervalMs(),
		BackoffFirstMs: ts.Backoff().FirstMs,
		BackoffMaxMs:   ts.Backoff().MaxMs,
		BackoffFactor:  ts.Backoff().Factor,

		Jitter:      string(ts.Backoff().Jitter),
		RestartType: string(ts.RestartType()),

		Targets:      ts.Targets(),
		TargetLabels: ts.TargetLabels(),
		RunnerLabels: ts.RunnerLabels(),

		CreatedAt: ts.CreatedAt().Format(time.RFC3339),
		UpdatedAt: ts.UpdatedAt().Format(time.RFC3339),
	}
	if p, err := proxy.SpecToProto(ts); err == nil {
		if preview, perr := proxy.CreateSpecWirePreview(p); perr == nil && len(preview) > 0 {
			out.CreateSpec = json.RawMessage(preview)
		}
	}
	return out
}

// RolloutSpecToREST builds the composite spec+rollouts REST DTO, flipping
// the `DirtyForRollout` flag when any rollout lags behind the spec generation.
func RolloutSpecToREST(ts *model.Spec, states []*model.Rollout) restv1.RolloutSpec {
	out := restv1.RolloutSpec{Spec: SpecToREST(ts)}
	if ts != nil {
		gen := ts.Generation()
		for _, ss := range states {
			if ss.IsStaleFor(gen) {
				out.Spec.DirtyForRollout = true
				break
			}
		}
	}
	if len(states) > 0 {
		out.Entries = make([]restv1.RolloutEntry, 0, len(states))
		for _, ss := range states {
			out.Entries = append(out.Entries, RolloutEntryToREST(ss))
		}
	}
	return out
}

// RolloutEntryToREST maps a domain Rollout to a REST rollout entry.
func RolloutEntryToREST(ss *model.Rollout) restv1.RolloutEntry {
	if ss == nil {
		return restv1.RolloutEntry{}
	}
	out := restv1.RolloutEntry{
		Status:             ss.Status().String(),
		Intent:             ss.Intent().String(),
		DesiredGeneration:  ss.DesiredGeneration(),
		ObservedGeneration: ss.ObservedGeneration(),
		Attempts:           ss.Attempts(),
		AgentID:            ss.AgentID(),
		ActualTaskID:       ss.ActualTaskID(),
	}
	if !ss.LastPushedAt().IsZero() {
		out.LastPushedAt = ss.LastPushedAt().Format(time.RFC3339)
	}
	if !ss.LastSyncedAt().IsZero() {
		out.LastSyncedAt = ss.LastSyncedAt().Format(time.RFC3339)
	}
	if ss.Error() != "" {
		out.Error = ss.Error()
	}
	return out
}

// roleNameByID maps built-in role IDs to display names. Unknown IDs are
// surfaced as themselves by UserToREST.
var roleNameByID = func() map[string]string {
	m := make(map[string]string, len(kind.BuiltinRoles))
	for _, r := range kind.BuiltinRoles {
		m[r.ID] = r.Name
	}
	return m
}()
