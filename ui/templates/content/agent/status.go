package agent

import (
	"strings"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/ui/templates/component/visual"
)

// agentDotColor returns the StatusDot color for a kind.AgentStatus.
func agentDotColor(s kind.AgentStatus) string {
	switch s {
	case kind.AgentStatusActive:
		return "success"
	case kind.AgentStatusInactive:
		return "muted"
	case kind.AgentStatusDisconnected:
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
