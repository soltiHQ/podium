package restv1

type User struct {
	Permissions []string `json:"permissions,omitempty"`
	RoleIDs     []string `json:"role_ids,omitempty"`

	ID      string `json:"id"`
	Subject string `json:"subject"`
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`

	Disabled bool `json:"disabled"`
}

type UserListResponse struct {
	Items      []User `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}
