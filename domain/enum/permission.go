package enum

import "slices"

// Permission defines a stable identifier of an allowed action.
type Permission string

// Known reports whether p is a declared permission, i.e. listed in All.
// Stored strings outside All grant nothing.
func (p Permission) Known() bool {
	return slices.Contains(All, p)
}

const (
	AgentsGet  Permission = "agents:get"
	AgentsEdit Permission = "agents:edit"

	// AgentsReset issues an agent a new credential, which ends the one it holds,
	// and AgentsDelete removes an agent and its rollouts. Of the built-in roles
	// only Admin holds them, as it holds All; a user who holds every permission
	// directly does not gain them.
	AgentsDelete Permission = "agents:delete"
	AgentsReset  Permission = "agents:reset"

	UsersGet    Permission = "users:get"
	UsersAdd    Permission = "users:add"
	UsersEdit   Permission = "users:edit"
	UsersDelete Permission = "users:delete"

	SpecsGet    Permission = "taskspecs:get"
	SpecsAdd    Permission = "taskspecs:add"
	SpecsEdit   Permission = "taskspecs:edit"
	SpecsDeploy Permission = "taskspecs:deploy"
	SpecsDelete Permission = "taskspecs:delete"

	// ClusterGet reads the status of a raft cluster, and ClusterManage adds and
	// removes its members. Both exist in every mode, and guard only the cluster
	// routes, which a raft node alone serves.
	ClusterGet    Permission = "cluster:get"
	ClusterManage Permission = "cluster:manage"
)

// All contains all declared permissions.
var All = []Permission{
	AgentsGet,
	AgentsEdit,
	AgentsDelete,
	AgentsReset,

	UsersGet,
	UsersAdd,
	UsersEdit,
	UsersDelete,

	SpecsGet,
	SpecsAdd,
	SpecsEdit,
	SpecsDeploy,
	SpecsDelete,

	ClusterGet,
	ClusterManage,
}
