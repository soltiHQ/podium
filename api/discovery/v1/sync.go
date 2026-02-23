package discoveryv1

// SyncRequest is the payload agents send periodically to report their state.
type SyncRequest struct {
	UptimeSeconds int64 `json:"uptime_seconds"`
	Ts            int64 `json:"ts,omitempty"`
	EndpointType  int   `json:"endpoint_type"`

	Metadata map[string]string `json:"metadata,omitempty"`

	ID         string `json:"id"`
	Name       string `json:"name"`
	Endpoint   string `json:"endpoint"`
	APIVersion int `json:"api_version,omitempty"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Platform   string `json:"platform"`
}

// SyncResponse is returned to the agent after a successful sync.
type SyncResponse struct {
	Success bool `json:"success"`
}
