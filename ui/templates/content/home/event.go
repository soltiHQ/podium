package home

import (
	"net/url"
	"time"

	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
)

// IssueGroup aggregates identical issues into a single row with a count.
type IssueGroup struct {
	Time time.Time

	Count int
	Kind  string

	Payload event.Payload
}

// GroupIssues aggregates events by kind + payload ID.
// Input must be in reverse chronological order (newest first).
func GroupIssues(events []event.Record) []IssueGroup {
	type gkey struct{ kind, id string }

	var (
		idx    = map[gkey]int{}
		groups []IssueGroup
	)
	for _, ev := range events {
		k := gkey{ev.Kind, ev.Payload.ID}
		if i, ok := idx[k]; ok {
			groups[i].Count++
		} else {
			idx[k] = len(groups)
			groups = append(groups, IssueGroup{
				Kind:    ev.Kind,
				Payload: ev.Payload,
				Count:   1,
				Time:    ev.Time,
			})
		}
	}
	return groups
}

// issueDeleteURL builds the hx-delete URL for an issue group.
func issueDeleteURL(g IssueGroup) string {
	v := url.Values{}
	v.Set("kind", g.Kind)
	v.Set("entity", g.Payload.ID)
	v.Set("name", g.Payload.Name)
	return routepath.ApiDashboardIssues + "?" + v.Encode()
}

// issueDescription builds a one-liner for a grouped issue.
func issueDescription(g IssueGroup) string {
	return eventEntity(g.Kind) + " " + g.Payload.DisplayName()
}

// issueBorderColor returns the left border accent class for an issue event.
func issueBorderColor(kind string) string {
	switch kind {
	case event.AgentDisconnected, event.AgentDeleted, event.RateLimited,
		event.SyncFailed:
		return "border-l-danger"
	case event.AgentInactive:
		return "border-l-warning"
	default:
		return "border-l-border"
	}
}

// eventLabelColor returns the text color class for an event label.
func eventLabelColor(kind string) string {
	switch kind {
	case event.AgentConnected, event.SessionCreated:
		return "text-success"
	case event.AgentDisconnected, event.AgentDeleted,
		event.UserDeleted, event.RateLimited, event.SyncFailed:
		return "text-danger"
	case event.AgentInactive:
		return "text-warning"
	case event.SpecCreated, event.UserCreated:
		return "text-primary"
	case event.SpecUpdated, event.SpecDeployed,
		event.UserUpdated, event.UserPasswordChanged, event.UserStatusChanged:
		return "text-secondary"
	default:
		return "text-muted"
	}
}

// eventLabel returns a short verb for the event kind.
func eventLabel(kind string) string {
	switch kind {
	case event.AgentConnected:
		return "connected"
	case event.AgentInactive:
		return "inactive"
	case event.AgentDisconnected:
		return "disconnected"
	case event.AgentDeleted:
		return "deleted"
	case event.SpecCreated:
		return "created"
	case event.SpecUpdated:
		return "updated"
	case event.SpecDeployed:
		return "deployed"
	case event.UserCreated:
		return "created"
	case event.UserUpdated:
		return "updated"
	case event.UserDeleted:
		return "deleted"
	case event.UserPasswordChanged:
		return "password changed"
	case event.UserStatusChanged:
		return "status changed"
	case event.SessionCreated:
		return "logged in"
	case event.RateLimited:
		return "rate limited"
	case event.SyncFailed:
		return "sync failed"
	case event.IssueClosed:
		return "closed"
	default:
		return kind
	}
}

// eventEntity returns the lowercase entity type for an event kind.
func eventEntity(kind string) string {
	switch kind {
	case event.AgentConnected, event.AgentInactive,
		event.AgentDisconnected, event.AgentDeleted:
		return "agent"
	case event.SpecCreated, event.SpecUpdated, event.SpecDeployed,
		event.SyncFailed:
		return "spec"
	case event.UserCreated, event.UserUpdated, event.UserDeleted,
		event.UserPasswordChanged, event.UserStatusChanged,
		event.SessionCreated, event.RateLimited:
		return "user"
	case event.IssueClosed:
		return "issue"
	default:
		return ""
	}
}

// eventActor returns the actor name when it differs from the target, empty otherwise.
func eventActor(ev event.Record) string {
	if ev.Payload.By != "" && ev.Payload.By != ev.Payload.DisplayName() {
		return ev.Payload.By
	}
	return ""
}

// word returns singular or plural form based on count.
func word(n int, singular, pl string) string {
	if n == 1 {
		return singular
	}
	return pl
}
