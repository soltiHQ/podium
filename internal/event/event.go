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

// Refresh triggers are coarse "this view changed, re-fetch it" pulses sent via
// [Hub.Notify]. They reach the browser as SSE HX-Trigger events and drive the
// HTMX polling containers. Two distinct channels share this Hub:
//
//   - Refresh* (Notify)            → transient UI refresh signal, not stored.
//   - *Created / *Updated / …      → persisted activity-feed entries (Record).
//
// Values are the on-the-wire HX-Trigger names; templates reference them through
// the htmx package, which aliases these constants.
const (
	RefreshDashboard = "dashboard_update"
	RefreshAgents    = "agent_update"
	RefreshSpecs     = "spec_update"
	RefreshUsers     = "user_update"
	RefreshSessions  = "session_update"
)

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

// Subject is the kind of entity an event is about. The dashboard derives from
// it which permission reads an event and which one dismisses an issue.
type Subject uint8

const (
	// SubjectUnknown covers unknown kinds and kinds that can name any subject.
	// Callers must require the permissions of every subject.
	SubjectUnknown Subject = iota
	SubjectAgent
	SubjectSpec
	// SubjectUser covers users, sessions and authentication.
	SubjectUser
)

// SubjectOf classifies an event kind. IssueClosed is SubjectUnknown: a closed
// issue can name an agent, a spec or a login subject.
func SubjectOf(kind string) Subject {
	switch kind {
	case AgentConnected, AgentInactive, AgentDisconnected, AgentDeleted:
		return SubjectAgent
	case SpecCreated, SpecUpdated, SpecDeployed, SyncFailed:
		return SubjectSpec
	case UserCreated, UserUpdated, UserDeleted, UserPasswordChanged, UserStatusChanged,
		SessionCreated, RateLimited:
		return SubjectUser
	default:
		return SubjectUnknown
	}
}
