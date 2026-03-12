// Package event provides an event broadcast hub with ring-buffered activity recording and Server-Sent Events streaming.
package event

import "time"

// Payload describes who did what to whom.
type Payload struct {
	ID     string
	By     string
	Name   string
	Detail string
}

// DisplayName returns Name if set, otherwise falls back to ID.
func (p Payload) DisplayName() string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// Record is a single entry in the ring buffer.
type Record struct {
	Time    time.Time
	Kind    string
	Payload Payload
}

// Event kinds for the dashboard activity feed.
const (
	AgentConnected    = "agent_connected"
	AgentInactive     = "agent_inactive"
	AgentDisconnected = "agent_disconnected"
	AgentDeleted      = "agent_deleted"

	SpecCreated  = "spec_created"
	SpecUpdated  = "spec_updated"
	SpecDeployed = "spec_deployed"

	UserCreated         = "user_created"
	UserUpdated         = "user_updated"
	UserDeleted         = "user_deleted"
	UserPasswordChanged = "user_password_changed"
	UserStatusChanged   = "user_status_changed"

	SessionCreated = "session_created"

	RateLimited = "rate_limited"

	IssueClosed = "issue_closed"

	SyncFailed = "sync_failed"
)

// issueKinds defines which event kinds are classified as issues.
var issueKinds = map[string]struct{}{
	AgentDisconnected: {},
	AgentInactive:     {},
	AgentDeleted:      {},
	RateLimited:       {},
	SyncFailed:        {},
}

// IsIssueKind reports whether the event kind is classified as an issue.
func IsIssueKind(kind string) bool {
	_, ok := issueKinds[kind]
	return ok
}
