package trigger

const (
	Header = "HX-Trigger"

	UserUpdate        = "user_update"
	UserSessionUpdate = "user_session_update"
)

const (
	Every30s = "every 30s"
	Every1m  = "every 60s"
	Every5m  = "every 300s"

	UserSessionsRefresh = Every1m
)
