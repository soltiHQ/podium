package wire

import "github.com/soltiHQ/control-plane/domain/enum"

// Small helpers so each DTO can cast raw primitives to kind.* without
// duplicating the conversions.

func kindEndpointType(s string) enum.EndpointType { return enum.EndpointType(s) }
func kindAPIVersion(v uint8) enum.APIVersion      { return enum.APIVersion(v) }
func kindAgentStatus(v uint8) enum.AgentStatus    { return enum.AgentStatus(v) }
func kindAuth(s string) enum.Auth                 { return enum.Auth(s) }
func kindSyncStatus(v uint8) enum.SyncStatus      { return enum.SyncStatus(v) }
func kindRolloutIntent(v uint8) enum.RolloutIntent {
	return enum.RolloutIntent(v)
}
func kindTaskKindType(s string) enum.TaskKindType { return enum.TaskKindType(s) }
func kindRestartType(s string) enum.RestartType   { return enum.RestartType(s) }

// kindPermissionsFromStrings converts persisted string permissions back to
// enum.Permission slice.
func kindPermissionsFromStrings(ss []string) []enum.Permission {
	out := make([]enum.Permission, 0, len(ss))
	for _, s := range ss {
		out = append(out, enum.Permission(s))
	}
	return out
}
