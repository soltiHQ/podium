package restv1

type Role struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type RoleListResponse struct {
	Items []Role `json:"items"`
}
