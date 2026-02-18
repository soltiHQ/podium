package trigger

import "net/http"

const (
	Header         = "HX-Trigger"
	RedirectHeader = "HX-Redirect"

	UserUpdate        = "user_update"
	UserSessionUpdate = "user_session_update"
)

const (
	Every30s = "every 30s"
	Every1m  = "every 60s"
	Every5m  = "every 300s"

	UserSessionsRefresh = Every1m
)

// TODO: USE IT!!!

// Set sets an HX-Trigger header on the response.
func Set(w http.ResponseWriter, event string) {
	w.Header().Set(Header, event)
}

// Redirect sets an HX-Redirect header on the response.
func Redirect(w http.ResponseWriter, url string) {
	w.Header().Set(RedirectHeader, url)
}
