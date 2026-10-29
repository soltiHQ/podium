package agent

import (
	"strings"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/ui/templates/component/visual"
)

// agentDotColor returns the StatusDot color for a enum.AgentStatus.
func agentDotColor(s enum.AgentStatus) string {
	switch s {
	case enum.AgentStatusActive:
		return "success"
	case enum.AgentStatusInactive:
		return "muted"
	case enum.AgentStatusDisconnected:
		return "danger"
	default:
		return "muted"
	}
}

// agentDotColorStr returns the StatusDot color for a string status (REST API).
func agentDotColorStr(s string) string {
	switch s {
	case "active":
		return "success"
	case "inactive":
		return "muted"
	case "disconnected":
		return "danger"
	default:
		return "muted"
	}
}

// agentBadgeVariant maps a dot color to a badge variant.
func agentBadgeVariant(dotColor string) visual.Variant {
	switch dotColor {
	case "success":
		return visual.VariantSuccess
	case "danger":
		return visual.VariantDanger
	default:
		return visual.VariantMuted
	}
}

// agentStatusLabel returns a capitalized label for a status string.
func agentStatusLabel(s string) string {
	if s == "" {
		return "Unknown"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
