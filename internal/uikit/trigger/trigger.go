// Package trigger defines HTMX event names and polling intervals
// used to coordinate client-side reactivity across the UI.
package trigger

import "net/http"

const (
	Header         = "HX-Trigger"
	RedirectHeader = "HX-Redirect"

	SessionUpdate = "session_update"
	SpecUpdate    = "spec_update"
	UserUpdate    = "user_update"
	AgentUpdate   = "agent_update"
)

const (
	Every5s  = "every 5s"
	Every15s = "every 15s"
	Every30s = "every 30s"
	Every1m  = "every 60s"
)

// Config holds configurable polling intervals.
type Config struct {
	UsersRefresh        string
	UserDetailRefresh   string
	UserSessionsRefresh string
	AgentsRefresh       string
	AgentDetailRefresh  string
	AgentTasksRefresh   string
	SpecsRefresh        string
	SpecDetailRefresh   string
}

var cfg = defaultConfig()

func defaultConfig() Config {
	return Config{
		UsersRefresh:        Every1m,
		UserDetailRefresh:   Every1m,
		UserSessionsRefresh: Every30s,

		AgentsRefresh:      Every15s,
		AgentDetailRefresh: Every30s,
		AgentTasksRefresh:  Every5s,
		
		SpecsRefresh:      Every5s,
		SpecDetailRefresh: Every15s,
	}
}

// Configure overrides default polling intervals. Must be called before server start.
func Configure(c Config) {
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

// Set sets an HX-Trigger header on the response.
func Set(w http.ResponseWriter, event string) {
	w.Header().Set(Header, event)
}

// Redirect sets an HX-Redirect header on the response.
func Redirect(w http.ResponseWriter, url string) {
	w.Header().Set(RedirectHeader, url)
}
