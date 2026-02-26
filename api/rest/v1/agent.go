package restv1

type Agent struct {
	UptimeSeconds int64             `json:"uptime_seconds"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`

	ID           string `json:"id"`
	Name         string `json:"name"`
	Endpoint     string `json:"endpoint"`
	EndpointType string `json:"endpoint_type"`
	APIVersion   string `json:"api_version"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Platform     string `json:"platform"`

	Status            string `json:"status"`
	LastSeenAt        string `json:"last_seen_at,omitempty"`
	HeartbeatInterval int    `json:"heartbeat_interval_s,omitempty"`
}

type AgentListResponse struct {
	Items      []Agent `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type AgentPatchLabelsRequest struct {
	Labels map[string]string `json:"labels"`
}
