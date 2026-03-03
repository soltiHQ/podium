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
	Every5m  = "every 300s"
)

// Config holds configurable polling intervals.
type Config struct {
	UserSessionsRefresh string
	AgentTasksRefresh   string
	SpecsRefresh        string
}

var cfg = defaultConfig()

func defaultConfig() Config {
	return Config{
		UserSessionsRefresh: Every1m,
		AgentTasksRefresh:   Every15s,
		SpecsRefresh:        Every5s,
	}
}

// Configure overrides default polling intervals. Must be called before server start.
func Configure(c Config) {
	if c.UserSessionsRefresh != "" {
		cfg.UserSessionsRefresh = c.UserSessionsRefresh
	}
	if c.AgentTasksRefresh != "" {
		cfg.AgentTasksRefresh = c.AgentTasksRefresh
	}
	if c.SpecsRefresh != "" {
		cfg.SpecsRefresh = c.SpecsRefresh
	}
}

// GetUserSessionsRefresh returns the polling interval for user session lists.
func GetUserSessionsRefresh() string { return cfg.UserSessionsRefresh }

// GetAgentTasksRefresh returns the polling interval for agent task lists.
func GetAgentTasksRefresh() string { return cfg.AgentTasksRefresh }

// GetSpecsRefresh returns the polling interval for spec lists.
func GetSpecsRefresh() string { return cfg.SpecsRefresh }

// Set sets an HX-Trigger header on the response.
func Set(w http.ResponseWriter, event string) {
	w.Header().Set(Header, event)
}

// Redirect sets an HX-Redirect header on the response.
func Redirect(w http.ResponseWriter, url string) {
	w.Header().Set(RedirectHeader, url)
}
