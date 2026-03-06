package home

import "github.com/soltiHQ/control-plane/internal/uikit/trigger"

// issueBorderColor returns the left border accent class for an issue event.
func issueBorderColor(kind string) string {
	switch kind {
	case trigger.EventAgentDisconnected, trigger.EventAgentDeleted:
		return "border-l-danger"
	case trigger.EventAgentInactive:
		return "border-l-warning"
	default:
		return "border-l-border"
	}
}

// eventLabelColor returns the text color class for an event label.
func eventLabelColor(kind string) string {
	switch kind {
	case trigger.EventAgentConnected:
		return "text-success"
	case trigger.EventAgentDisconnected, trigger.EventAgentDeleted, trigger.EventUserDeleted:
		return "text-danger"
	case trigger.EventAgentInactive:
		return "text-warning"
	case trigger.EventSpecCreated, trigger.EventUserCreated:
		return "text-primary"
	case trigger.EventSpecUpdated, trigger.EventSpecDeployed:
		return "text-secondary"
	default:
		return "text-muted"
	}
}

// eventLabel returns a short verb for the event kind.
func eventLabel(kind string) string {
	switch kind {
	case trigger.EventAgentConnected:
		return "connected"
	case trigger.EventAgentInactive:
		return "inactive"
	case trigger.EventAgentDisconnected:
		return "disconnected"
	case trigger.EventAgentDeleted:
		return "deleted"
	case trigger.EventSpecCreated:
		return "created"
	case trigger.EventSpecUpdated:
		return "updated"
	case trigger.EventSpecDeployed:
		return "deployed"
	case trigger.EventUserCreated:
		return "created"
	case trigger.EventUserDeleted:
		return "deleted"
	default:
		return kind
	}
}

// eventEntity returns the entity type prefix for an event.
func eventEntity(kind string) string {
	switch kind {
	case trigger.EventAgentConnected, trigger.EventAgentInactive,
		trigger.EventAgentDisconnected, trigger.EventAgentDeleted:
		return "Agent"
	case trigger.EventSpecCreated, trigger.EventSpecUpdated, trigger.EventSpecDeployed:
		return "Spec"
	case trigger.EventUserCreated, trigger.EventUserDeleted:
		return "User"
	default:
		return ""
	}
}

// eventName returns the entity name from an event.
func eventName(ev trigger.EventRecord) string {
	name := ev.Payload["name"]
	if name == "" {
		name = ev.Payload["id"]
	}
	if name == "" {
		name = ev.Payload["subject"]
	}
	return name
}

// word returns singular or plural form based on count.
func word(n int, singular, pl string) string {
	if n == 1 {
		return singular
	}
	return pl
}
