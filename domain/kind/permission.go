package kind

// Permission defines a stable identifier of an allowed action.
type Permission string

const (
	AgentsGet  Permission = "agents:get"
	AgentsEdit Permission = "agents:edit"

	UsersGet    Permission = "users:get"
	UsersAdd    Permission = "users:add"
	UsersEdit   Permission = "users:edit"
	UsersDelete Permission = "users:delete"

	SpecsGet    Permission = "taskspecs:get"
	SpecsAdd    Permission = "taskspecs:add"
	SpecsEdit   Permission = "taskspecs:edit"
	SpecsDeploy Permission = "taskspecs:deploy"
	SpecsDelete Permission = "taskspecs:delete"
)

// All contains all declared permissions.
var All = []Permission{
	AgentsGet,
	AgentsEdit,

	UsersGet,
	UsersAdd,
	UsersEdit,
	UsersDelete,

	SpecsGet,
	SpecsAdd,
	SpecsEdit,
	SpecsDeploy,
	SpecsDelete,
}
