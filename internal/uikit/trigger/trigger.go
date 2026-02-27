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

	UserSessionsRefresh = Every1m
	AgentTasksRefresh   = Every15s
	SpecsRefresh        = Every5s
)

// Set sets an HX-Trigger header on the response.
func Set(w http.ResponseWriter, event string) {
	w.Header().Set(Header, event)
}

// Redirect sets an HX-Redirect header on the response.
func Redirect(w http.ResponseWriter, url string) {
	w.Header().Set(RedirectHeader, url)
}
