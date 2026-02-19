package routepath

const (
	PageHome       = "/"
	PageLogin      = "/login"
	PageLogout     = "/logout"
	PageUsers      = "/users"
	PageAgents     = "/agents"
	PageAgentInfo  = "/agents/info/"
	PageUserInfo   = "/users/info/"
	PageUserDelete = "/users/delete/"
	PageUserEdit   = "/users/edit/"
	PageUserNew    = "/users/new/"

	ApiSession     = "/api/v1/session/"
	ApiUser        = "/api/v1/users/"
	ApiUsers       = "/api/v1/users"
	ApiAgents      = "/api/v1/agents"
	ApiAgent       = "/api/v1/agents/"
	ApiPermissions = "/api/v1/permissions"
	ApiRoles       = "/api/v1/roles"
)

var (
	PageUserDeleteByID = func(id string) string { return PageUserDelete + id }
	PageUserEditByID   = func(id string) string { return PageUserEdit + id }
	PageUserInfoByID   = func(id string) string {
		return PageUserInfo + id
	}

	PageAgentInfoByID = func(id string) string { return PageAgentInfo + id }
	ApiAgentByID      = func(id string) string { return ApiAgent + id }
	ApiAgentLabels    = func(id string) string { return ApiAgent + id + "/labels" }
	ApiAgentTasks     = func(id string) string { return ApiAgent + id + "/tasks" }

	ApiUserRevokeSession = func(id string) string {
		return ApiSession + id + "/revoke"
	}
	ApiUserEnable = func(id string) string {
		return ApiUser + id + "/enable"
	}
	ApiUserDisable = func(id string) string {
		return ApiUser + id + "/disable"
	}
	ApiUserCrudOp = func(id string) string {
		return ApiUser + id
	}
	ApiUserSessions = func(id string) string {
		return ApiUser + id + "/sessions"
	}
	ApiUserPassword = func(id string) string {
		return ApiUser + id + "/password"
	}
)
