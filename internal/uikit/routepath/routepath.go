package routepath

import "net/url"

const (
	PageHome   = "/"
	PageLogin  = "/login"
	PageLogout = "/logout"

	PageUsers    = "/users"
	PageUserInfo = "/users/info/"

	PageHistory = "/history"

	PageAgents    = "/agents"
	PageAgentInfo = "/agents/info/"

	PageSpecs    = "/specs"
	PageSpecNew  = "/specs/new"
	PageSpecEdit = "/specs/edit/"
	PageSpecInfo = "/specs/info/"

	ApiSession = "/api/v1/session/"

	ApiUsers = "/api/v1/users"
	ApiUser  = "/api/v1/users/"

	ApiHistory = "/api/v1/history"

	ApiAgents = "/api/v1/agents"
	ApiAgent  = "/api/v1/agents/"

	ApiPermissions = "/api/v1/permissions"
	ApiRoles       = "/api/v1/roles"

	ApiSpecs = "/api/v1/specs"
	ApiSpec  = "/api/v1/specs/"

	ApiDashboard       = "/api/v1/dashboard"
	ApiDashboardIssues = "/api/v1/dashboard/issues"
	ApiEventStream     = "/api/v1/events/stream"
)

// Every ID below is data: each helper path-escapes it as one URL segment, so
// it can neither add segments, a query or a fragment, nor break out of the
// HTML attribute or JavaScript string the URL is later placed in. The server
// routes on the decoded r.URL.Path and sees the original ID.

// Users.

func PageUserInfoByID(id string) string     { return PageUserInfo + url.PathEscape(id) }
func ApiUserByID(id string) string          { return ApiUser + url.PathEscape(id) }
func ApiUserEnable(id string) string        { return ApiUser + url.PathEscape(id) + "/enable" }
func ApiUserDisable(id string) string       { return ApiUser + url.PathEscape(id) + "/disable" }
func ApiUserSessions(id string) string      { return ApiUser + url.PathEscape(id) + "/sessions" }
func ApiUserPassword(id string) string      { return ApiUser + url.PathEscape(id) + "/password" }
func ApiUserRevokeSession(id string) string { return ApiSession + url.PathEscape(id) + "/revoke" }

// Agents.

func PageAgentInfoByID(id string) string { return PageAgentInfo + url.PathEscape(id) }
func ApiAgentByID(id string) string      { return ApiAgent + url.PathEscape(id) }
func ApiAgentLabels(id string) string    { return ApiAgent + url.PathEscape(id) + "/labels" }
func ApiAgentTasks(id string) string     { return ApiAgent + url.PathEscape(id) + "/tasks" }

func ApiAgentTaskLogs(agentID, taskID string, uid ...string) string {
	path := ApiAgent + url.PathEscape(agentID) + "/tasks/" + url.PathEscape(taskID) + "/logs/stream"
	if len(uid) > 0 && uid[0] != "" {
		path += "?task_uid=" + url.QueryEscape(uid[0])
	}
	return path
}

// Specs.

func PageSpecInfoByID(id string) string   { return PageSpecInfo + url.PathEscape(id) }
func PageSpecEditByID(id string) string   { return PageSpecEdit + url.PathEscape(id) }
func ApiSpecByID(id string) string        { return ApiSpec + url.PathEscape(id) }
func ApiSpecDeploy(id string) string      { return ApiSpec + url.PathEscape(id) + "/deploy" }
func ApiSpecPlacement(id string) string   { return ApiSpec + url.PathEscape(id) + "/placement" }
func ApiSpecSync(id string) string        { return ApiSpec + url.PathEscape(id) + "/sync" }
func ApiSpecForceDelete(id string) string { return ApiSpec + url.PathEscape(id) + "/force" }

// CursorURL appends optional cursor and query parameters to a base API path.
func CursorURL(base, cursor, q string) string {
	v := url.Values{}
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	if q != "" {
		v.Set("q", q)
	}
	qs := v.Encode()
	if qs == "" {
		return base
	}
	return base + "?" + qs
}

func ApiAgentTask(agentID, taskID string) string {
	return ApiAgent + url.PathEscape(agentID) + "/tasks/" + url.PathEscape(taskID)
}

func PageHistoryByAgent(id string) string { return PageHistory + "?agent_id=" + url.QueryEscape(id) }
