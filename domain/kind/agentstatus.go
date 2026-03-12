package kind

// AgentStatus describes the lifecycle state of an agent.
type AgentStatus uint8

const (
	AgentStatusActive AgentStatus = iota
	AgentStatusInactive
	AgentStatusDisconnected
)

// String returns the human-readable status label.
func (s AgentStatus) String() string {
	switch s {
	case AgentStatusActive:
		return "active"
	case AgentStatusInactive:
		return "inactive"
	case AgentStatusDisconnected:
		return "disconnected"
	default:
		return "unknown"
	}
}
