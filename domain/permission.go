package domain

// Permission is a stable identifier of an allowed action.
type Permission string

const (
	PermAgentsGet  Permission = "agents:get"
	PermAgentsEdit Permission = "agents:edit"

	PermUsersGet    Permission = "users:get"
	PermUsersAdd    Permission = "users:add"
	PermUsersEdit   Permission = "users:edit"
	PermUsersDelete Permission = "users:delete"
)

// PermissionsAll is useful for bootstrap (admin) and validation.
var PermissionsAll = []Permission{
	PermAgentsGet,
	PermAgentsEdit,
	PermUsersGet,
	PermUsersAdd,
	PermUsersEdit,
	PermUsersDelete,
}
