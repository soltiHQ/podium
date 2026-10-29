package restv1

// Role is the REST representation of a permission role.
type Role struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListRolesResponse lists the roles the caller may grant (bounded; not paginated).
type ListRolesResponse struct {
	Items []Role `json:"items"`
}
