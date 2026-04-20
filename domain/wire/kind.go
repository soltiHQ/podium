package wire

import "github.com/soltiHQ/control-plane/domain/kind"

// Small helpers so each DTO can cast raw primitives to kind.* without
// duplicating the conversions.

func kindEndpointType(s string) kind.EndpointType { return kind.EndpointType(s) }
func kindAPIVersion(v uint8) kind.APIVersion      { return kind.APIVersion(v) }
func kindAgentStatus(v uint8) kind.AgentStatus    { return kind.AgentStatus(v) }
func kindAuth(s string) kind.Auth                 { return kind.Auth(s) }
func kindSyncStatus(v uint8) kind.SyncStatus      { return kind.SyncStatus(v) }
func kindRolloutIntent(v uint8) kind.RolloutIntent {
	return kind.RolloutIntent(v)
}
func kindTaskKindType(s string) kind.TaskKindType { return kind.TaskKindType(s) }
func kindRestartType(s string) kind.RestartType   { return kind.RestartType(s) }

// kindPermissionsFromStrings converts persisted string permissions back to
// kind.Permission slice.
func kindPermissionsFromStrings(ss []string) []kind.Permission {
	out := make([]kind.Permission, 0, len(ss))
	for _, s := range ss {
		out = append(out, kind.Permission(s))
	}
	return out
}
