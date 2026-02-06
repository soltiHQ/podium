package storage

// AgentFilterSeal is a marker type used to tag valid AgentFilter implementations.
type AgentFilterSeal struct{}

// UserFilterSeal is a marker type used to tag valid UserFilter implementations.
type UserFilterSeal struct{}

// RoleFilterSeal is a marker type used to tag valid RoleFilter implementations.
type RoleFilterSeal struct{}

// AgentFilter defines a storage-specific query abstraction for agents.
type AgentFilter interface {
	IsAgentFilter(AgentFilterSeal)
}

// UserFilter defines a storage-specific query abstraction for users.
type UserFilter interface {
	IsUserFilter(UserFilterSeal)
}

// RoleFilter defines a storage-specific query abstraction for roles.
type RoleFilter interface {
	IsRoleFilter(RoleFilterSeal)
}
