package routepath

const (
	PageHome       = "/"
	PageLogin      = "/login"
	PageLogout     = "/logout"
	PageUsers      = "/users"
	PageUserInfo   = "/users/info/"
	PageUserDelete = "/users/delete/"
	PageUserEdit   = "/users/edit/"
	PageUserNew    = "/users/new/"

	ApiSession = "/api/v1/session/"
	ApiUser    = "/api/v1/users/"
	ApiUsers   = "/api/v1/users"
)

var (
	PageUserDeleteByID = func(id string) string { return PageUserDelete + id }
	PageUserEditByID   = func(id string) string { return PageUserEdit + id }
	PageUserInfoByID   = func(id string) string {
		return PageUserInfo + id
	}

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
)
