// Package trigger defines HTMX event names, polling intervals, and a notification hub by SSE:
//   - Named events shared by HTMX and SSE
//   - Configurable polling intervals as fallback
//   - Hub broadcasts mutations to all connected browsers
//   - Set() combines HX-Trigger header with SSE broadcast.
package trigger

import "net/http"

const (
	Header         = "HX-Trigger"
	RedirectHeader = "HX-Redirect"

	SessionUpdate   = "session_update"
	AgentUpdate     = "agent_update"
	SpecUpdate      = "spec_update"
	UserUpdate      = "user_update"
	DashboardUpdate = "dashboard_update"
)

// Event kinds for the dashboard activity feed.
const (
	EventAgentConnected      = "agent_connected"
	EventAgentInactive       = "agent_inactive"
	EventAgentDisconnected   = "agent_disconnected"
	EventAgentDeleted        = "agent_deleted"
	EventSpecCreated         = "spec_created"
	EventSpecUpdated         = "spec_updated"
	EventSpecDeployed        = "spec_deployed"
	EventUserCreated         = "user_created"
	EventUserUpdated         = "user_updated"
	EventUserDeleted         = "user_deleted"
	EventUserPasswordChanged = "user_password_changed"
	EventUserStatusChanged   = "user_status_changed"
	EventSessionCreated      = "session_created"
	EventRateLimited         = "rate_limited"
	EventIssueClosed         = "issue_closed"
)

const (
	Every1m = "every 60s"
	Every3m = "every 180s"
	Every5m = "every 300s"
)

// Config holds configurable polling intervals.
type Config struct {
	DashboardRefresh    string `yaml:"dashboard_refresh"`
	UsersRefresh        string `yaml:"users_refresh"`
	UserDetailRefresh   string `yaml:"user_detail_refresh"`
	UserSessionsRefresh string `yaml:"user_sessions_refresh"`
	AgentsRefresh       string `yaml:"agents_refresh"`
	AgentDetailRefresh  string `yaml:"agent_detail_refresh"`
	AgentTasksRefresh   string `yaml:"agent_tasks_refresh"`
	SpecsRefresh        string `yaml:"specs_refresh"`
	SpecDetailRefresh   string `yaml:"spec_detail_refresh"`
}

var cfg = defaultConfig()

func defaultConfig() Config {
	return Config{
		DashboardRefresh: Every1m,

		UsersRefresh:        Every3m,
		UserDetailRefresh:   Every5m,
		UserSessionsRefresh: Every3m,

		AgentsRefresh:      Every1m,
		AgentDetailRefresh: Every3m,
		AgentTasksRefresh:  Every1m,

		SpecsRefresh:      Every3m,
		SpecDetailRefresh: Every1m,
	}
}

// Configure overrides default polling intervals. Must be called before server start.
func Configure(c Config) {
	if c.DashboardRefresh != "" {
		cfg.DashboardRefresh = c.DashboardRefresh
	}
	if c.UsersRefresh != "" {
		cfg.UsersRefresh = c.UsersRefresh
	}
	if c.UserDetailRefresh != "" {
		cfg.UserDetailRefresh = c.UserDetailRefresh
	}
	if c.UserSessionsRefresh != "" {
		cfg.UserSessionsRefresh = c.UserSessionsRefresh
	}
	if c.AgentsRefresh != "" {
		cfg.AgentsRefresh = c.AgentsRefresh
	}
	if c.AgentDetailRefresh != "" {
		cfg.AgentDetailRefresh = c.AgentDetailRefresh
	}
	if c.AgentTasksRefresh != "" {
		cfg.AgentTasksRefresh = c.AgentTasksRefresh
	}
	if c.SpecsRefresh != "" {
		cfg.SpecsRefresh = c.SpecsRefresh
	}
	if c.SpecDetailRefresh != "" {
		cfg.SpecDetailRefresh = c.SpecDetailRefresh
	}
}

// GetDashboardRefresh returns the polling interval for the dashboard.
func GetDashboardRefresh() string { return cfg.DashboardRefresh }

// GetUsersRefresh returns the polling interval for user lists.
func GetUsersRefresh() string { return cfg.UsersRefresh }

// GetUserDetailRefresh returns the polling interval for user detail identity.
func GetUserDetailRefresh() string { return cfg.UserDetailRefresh }

// GetUserSessionsRefresh returns the polling interval for user session lists.
func GetUserSessionsRefresh() string { return cfg.UserSessionsRefresh }

// GetAgentsRefresh returns the polling interval for agent lists.
func GetAgentsRefresh() string { return cfg.AgentsRefresh }

// GetAgentDetailRefresh returns the polling interval for agent detail identity.
func GetAgentDetailRefresh() string { return cfg.AgentDetailRefresh }

// GetAgentTasksRefresh returns the polling interval for agent task lists.
func GetAgentTasksRefresh() string { return cfg.AgentTasksRefresh }

// GetSpecsRefresh returns the polling interval for spec lists.
func GetSpecsRefresh() string { return cfg.SpecsRefresh }

// GetSpecDetailRefresh returns the polling interval for spec detail identity.
func GetSpecDetailRefresh() string { return cfg.SpecDetailRefresh }

// Poll returns an hx-trigger value combining a polling interval with an SSE event.
// Use on Results containers that handle periodic and event-driven refreshes.
func Poll(interval, event string) string {
	return interval + ", " + event + " from:body"
}

// PollMulti returns an hx-trigger value combining a polling interval with multiple SSE events.
func PollMulti(interval string, events ...string) string {
	s := interval
	for _, e := range events {
		s += ", " + e + " from:body"
	}
	return s
}

// LoadAndPoll returns an hx-trigger value that fires once on a load, then keeps
// refreshing via polling and SSE. Use on DetailPanel containers.
func LoadAndPoll(interval, event string) string {
	return "load, " + interval + ", " + event + " from:body"
}

// Redirect sets an HX-Redirect header on the response.
func Redirect(w http.ResponseWriter, url string) {
	w.Header().Set(RedirectHeader, url)
}

// Set sets an HX-Trigger header on the response and broadcasts the event to all connected SSE clients.
func Set(w http.ResponseWriter, event string) {
	w.Header().Set(Header, event)
	Notify(event)
}
