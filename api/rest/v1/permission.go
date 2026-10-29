package restv1

// ListPermissionsResponse lists the permissions the caller may grant (bounded; not paginated).
type ListPermissionsResponse struct {
	Items []string `json:"items"`
}
