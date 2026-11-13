package middleware

import "strings"

// sessionPath is routepath.ApiSession, the prefix of the one route that
// carries a session ID in its path: POST /api/v1/session/{id}/revoke. The
// transport packages do not import uikit, so the value is repeated here, and
// TestLoggedPathsHideTheSessionID pins the two together.
const sessionPath = "/api/v1/session/"

// logPath returns path as a log line names it. A session path has its session
// ID replaced by "{id}": the session_id cookie carries the same value, and with
// the refresh token it renews the session. Everything between the prefix and
// the last "/" goes, so no part of the ID is left whatever it holds. The
// segment after that "/" stays, such as revoke, and a path with no "/" after
// the prefix loses all of it.
func logPath(path string) string {
	rest, ok := strings.CutPrefix(path, sessionPath)
	if !ok || rest == "" {
		return path
	}
	if i := strings.LastIndexByte(rest, '/'); i >= 0 {
		return sessionPath + "{id}" + rest[i:]
	}
	return sessionPath + "{id}"
}
