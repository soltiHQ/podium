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
	case trigger.EventSpecUpdated, trigger.EventSpecDeployed,
		trigger.EventUserUpdated, trigger.EventUserPasswordChanged, trigger.EventUserStatusChanged:
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
	case trigger.EventUserUpdated:
		return "updated"
	case trigger.EventUserDeleted:
		return "deleted"
	case trigger.EventUserPasswordChanged:
		return "password changed"
	case trigger.EventUserStatusChanged:
		return "status changed"
	default:
		return kind
	}
}

// eventEntity returns the lowercase entity type for an event kind.
func eventEntity(kind string) string {
	switch kind {
	case trigger.EventAgentConnected, trigger.EventAgentInactive,
		trigger.EventAgentDisconnected, trigger.EventAgentDeleted:
		return "agent"
	case trigger.EventSpecCreated, trigger.EventSpecUpdated, trigger.EventSpecDeployed:
		return "spec"
	case trigger.EventUserCreated, trigger.EventUserUpdated, trigger.EventUserDeleted,
		trigger.EventUserPasswordChanged, trigger.EventUserStatusChanged:
		return "user"
	default:
		return ""
	}
}

// eventTarget returns the entity display name (or ID as fallback).
func eventTarget(ev trigger.EventRecord) string {
	if ev.Payload.Name != "" {
		return ev.Payload.Name
	}
	return ev.Payload.ID
}

// eventActor returns the actor name when it differs from the target, empty otherwise.
func eventActor(ev trigger.EventRecord) string {
	target := eventTarget(ev)
	if ev.Payload.By != "" && ev.Payload.By != target {
		return ev.Payload.By
	}
	return ""
}

// eventDescription builds a human-readable one-liner for the event.
// Examples: "user asd by Admin", "agent gpu-3".
func eventDescription(ev trigger.EventRecord) string {
	desc := eventEntity(ev.Kind) + " " + eventTarget(ev)
	if actor := eventActor(ev); actor != "" {
		desc += " by " + actor
	}
	return desc
}

// word returns singular or plural form based on count.
func word(n int, singular, pl string) string {
	if n == 1 {
		return singular
	}
	return pl
}
