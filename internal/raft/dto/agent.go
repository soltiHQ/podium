package dto

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
)

// AgentDTO is the serialisable mirror of model.Agent.
type AgentDTO struct {
	ID                  string
	Name                string
	Endpoint            string
	EndpointType        string // kind.EndpointType
	APIVersion          uint8  // kind.APIVersion
	OS                  string
	Arch                string
	Platform            string
	UptimeSeconds       int64
	HeartbeatIntervalNs int64
	Status              uint8 // kind.AgentStatus

	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastSeenAt time.Time
	StaleAt    time.Time

	Metadata     map[string]string
	Labels       map[string]string
	Capabilities []string
}

// AgentToDTO snapshots an Agent for replication.
func AgentToDTO(a *model.Agent) *AgentDTO {
	md := make(map[string]string, len(a.MetadataAll()))
	for k, v := range a.MetadataAll() {
		md[k] = v
	}
	lbl := make(map[string]string, len(a.LabelsAll()))
	for k, v := range a.LabelsAll() {
		lbl[k] = v
	}
	return &AgentDTO{
		ID:                  a.ID(),
		Name:                a.Name(),
		Endpoint:            a.Endpoint(),
		EndpointType:        string(a.EndpointType()),
		APIVersion:          uint8(a.APIVersion()),
		OS:                  a.OS(),
		Arch:                a.Arch(),
		Platform:            a.Platform(),
		UptimeSeconds:       a.UptimeSeconds(),
		HeartbeatIntervalNs: a.HeartbeatInterval().Nanoseconds(),
		Status:              uint8(a.Status()),
		CreatedAt:           a.CreatedAt(),
		UpdatedAt:           a.UpdatedAt(),
		LastSeenAt:          a.LastSeenAt(),
		StaleAt:             a.StaleAt(),
		Metadata:            md,
		Labels:              lbl,
		Capabilities:        a.Capabilities(),
	}
}

// AgentFromDTO reconstructs an Agent byte-for-byte from its serialised form.
//
// Uses the minimal NewAgent constructor plus field setters so the DTO is the
// source of truth for every piece of state. The constructor's own validation
// (non-empty ID) still applies; everything else comes from d.
func AgentFromDTO(d *AgentDTO) (*model.Agent, error) {
	if d == nil {
		return nil, nil
	}
	a, err := model.NewAgent(d.ID, d.Name, d.Endpoint)
	if err != nil {
		return nil, err
	}
	a.SetEndpointType(kindEndpointType(d.EndpointType))
	a.SetAPIVersion(kindAPIVersion(d.APIVersion))
	a.SetOS(d.OS)
	a.SetArch(d.Arch)
	a.SetPlatform(d.Platform)
	a.SetUptimeSeconds(d.UptimeSeconds)
	a.SetHeartbeatInterval(time.Duration(d.HeartbeatIntervalNs))
	a.SetStatus(kindAgentStatus(d.Status))
	a.SetCreatedAt(d.CreatedAt)
	a.SetUpdatedAt(d.UpdatedAt)
	a.SetLastSeenAt(d.LastSeenAt)
	a.SetStaleAt(d.StaleAt)
	a.SetMetadata(d.Metadata)
	a.SetLabels(d.Labels)
	a.SetCapabilities(d.Capabilities)
	return a, nil
}
