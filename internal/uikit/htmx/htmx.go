package htmx

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/soltiHQ/control-plane/internal/event"
)

const (
	Header         = "HX-Trigger"
	RedirectHeader = "HX-Redirect"
)

// Trigger-name aliases re-exported from internal/event.
const (
	SessionUpdate   = event.RefreshSessions
	AgentUpdate     = event.RefreshAgents
	SpecUpdate      = event.RefreshSpecs
	UserUpdate      = event.RefreshUsers
	DashboardUpdate = event.RefreshDashboard
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

// ErrInvalidRefresh indicates a polling interval that is not "every <n>s", "every
// <n>m" or "every <n>ms" (n may have decimals).
var ErrInvalidRefresh = errors.New("htmx: polling interval is not of the form \"every 30s\"")

var refreshPattern = regexp.MustCompile(`^every [0-9]+(\.[0-9]+)?(ms|s|m)$`)

// Validate reports a polling interval that hx-trigger would not read as one.
// An empty field is valid: it keeps its default.
func (c Config) Validate() error {
	for _, v := range []struct{ key, value string }{
		{"dashboard_refresh", c.DashboardRefresh},
		{"users_refresh", c.UsersRefresh},
		{"user_detail_refresh", c.UserDetailRefresh},
		{"user_sessions_refresh", c.UserSessionsRefresh},
		{"agents_refresh", c.AgentsRefresh},
		{"agent_detail_refresh", c.AgentDetailRefresh},
		{"agent_tasks_refresh", c.AgentTasksRefresh},
		{"specs_refresh", c.SpecsRefresh},
		{"spec_detail_refresh", c.SpecDetailRefresh},
	} {
		if v.value != "" && !refreshPattern.MatchString(v.value) {
			return fmt.Errorf("%w: %s = %q", ErrInvalidRefresh, v.key, v.value)
		}
	}
	return nil
}

// cfg is a process-wide singleton holding the active polling intervals.
//
// Contract:
//
//	 Written once at startup, read-only while serving - concurrent Get/Refresh reads are race-free without a lock:
//
//		startup (1 goroutine) │ serving (N goroutines)
//		 Configure(cfg)    ───┤── Get*Refresh(cfg) ──► read
//		   write           ───┤── Get*Refresh(cfg) ──► read
//		      ▲                          ▲
//		  only here                 reads only
var cfg = defaultConfig()

func defaultConfig() Config {
	return Config{
		DashboardRefresh: Every1m,

		UsersRefresh:        Every3m,
		UserDetailRefresh:   Every5m,
		UserSessionsRefresh: Every3m,

		AgentsRefresh:      Every1m,
		AgentDetailRefresh: Every1m,
		AgentTasksRefresh:  Every1m,

		SpecsRefresh:      Every3m,
		SpecDetailRefresh: Every1m,
	}
}

// Configure overrides default polling interval.
//
// Called once at startup.
// Empty fields keep their defaults.
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

// Poll returns a hx-trigger value combining a polling interval with an SSE event.
// Use on Results containers that handle periodic and event-driven refreshes.
func Poll(interval, event string) string {
	return interval + ", " + event + " from:body"
}

// PollMulti returns a hx-trigger value combining a polling interval with multiple SSE events.
func PollMulti(interval string, events ...string) string {
	s := interval
	for _, e := range events {
		s += ", " + e + " from:body"
	}
	return s
}

// LoadAndPoll returns a hx-trigger value that fires once on a load, then keeps refreshing via polling and SSE.
func LoadAndPoll(interval, event string) string {
	return "load, " + interval + ", " + event + " from:body"
}

// Trigger sets an HX-Trigger header on the response.
func Trigger(w http.ResponseWriter, event string) {
	w.Header().Set(Header, event)
}

// Redirect sets an HX-Redirect header on the response.
func Redirect(w http.ResponseWriter, url string) {
	w.Header().Set(RedirectHeader, url)
}
