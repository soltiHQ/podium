package v1

type Agent struct {
	UptimeSeconds int64             `json:"uptime_seconds"`
	Ts            int64             `json:"ts,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`

	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Platform string `json:"platform"`
	ID       string `json:"id"`
}

type AgentListResponse struct {
	Items      []Agent `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type AgentPatchLabelsRequest struct {
	Labels map[string]string `json:"labels"`
}

// AgentSyncResponse is the response for the discovery sync endpoint.
type AgentSyncResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}
