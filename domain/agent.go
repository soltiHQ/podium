package domain

import (
	"time"

	discoverv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
)

var _ Entity[*AgentModel] = (*AgentModel)(nil)

// AgentModel is a domain model that describes agent metadata.
type AgentModel struct {
	// raw data from discovery proto.
	raw *discoverv1.SyncRequest
	// updatedAt is the last time the agent object itself was modified.
	updatedAt time.Time
	// labels contain key/value data assigned by the control plane.
	labels map[string]string
}

// NewAgentModel creates a new agent model with the given raw proto.
func NewAgentModel(raw *discoverv1.SyncRequest) (*AgentModel, error) {
	if raw == nil {
		return nil, ErrNilSyncRequest
	}
	agent := &AgentModel{
		updatedAt: time.Now(),
		labels:    make(map[string]string),
		raw:       raw,
	}
	if err := agent.validate(); err != nil {
		return nil, err
	}
	return agent, nil
}

// ID returns the agent's unique identifier.
func (a *AgentModel) ID() string {
	return a.raw.Id
}

// Name returns the agent's name.
func (a *AgentModel) Name() string {
	return a.raw.Name
}

// Endpoint returns the agent's network address.
func (a *AgentModel) Endpoint() string {
	return a.raw.Endpoint
}

// UptimeSeconds returns the agent's uptime in seconds.
func (a *AgentModel) UptimeSeconds() int64 {
	return a.raw.UptimeSeconds
}

// OS returns the agent's operating system.
func (a *AgentModel) OS() string {
	return a.raw.Os
}

// Arch returns the agent's architecture.
func (a *AgentModel) Arch() string {
	return a.raw.Arch
}

// Platform returns the agent's platform.
func (a *AgentModel) Platform() string {
	return a.raw.Platform
}

// UpdatedAt returns the last time the agent object was modified.
func (a *AgentModel) UpdatedAt() time.Time {
	return a.updatedAt
}

// Metadata returns the metadata value for the given key.
func (a *AgentModel) Metadata(key string) (string, bool) {
	v, ok := a.raw.Metadata[key]
	return v, ok
}

// MetadataAll returns the agent's metadata.
func (a *AgentModel) MetadataAll() map[string]string {
	out := make(map[string]string, len(a.raw.Metadata))
	for k, v := range a.raw.Metadata {
		out[k] = v
	}
	return out
}

// Label returns a label value for the given key.
func (a *AgentModel) Label(key string) (string, bool) {
	v, ok := a.labels[key]
	return v, ok
}

// LabelsAll returns a copy of the agent's labels.
func (a *AgentModel) LabelsAll() map[string]string {
	out := make(map[string]string, len(a.labels))
	for k, v := range a.labels {
		out[k] = v
	}
	return out
}

// LabelAdd sets a label on the agent.
func (a *AgentModel) LabelAdd(key, value string) {
	a.labels[key] = value
	a.updatedAt = time.Now()
}

// LabelDelete removes a label from the agent.
func (a *AgentModel) LabelDelete(key string) {
	delete(a.labels, key)
	a.updatedAt = time.Now()
}

// Clone creates a deep copy of the agent model.
func (a *AgentModel) Clone() *AgentModel {
	var raw discoverv1.SyncRequest
	raw = *a.raw

	labels := make(map[string]string, len(a.labels))
	for k, v := range a.labels {
		labels[k] = v
	}
	if a.raw.Metadata != nil {
		metadata := make(map[string]string, len(a.raw.Metadata))
		for k, v := range a.raw.Metadata {
			metadata[k] = v
		}
		raw.Metadata = metadata
	}
	return &AgentModel{
		updatedAt: a.updatedAt,
		labels:    labels,
		raw:       &raw,
	}
}

func (a *AgentModel) validate() error {
	if err := validateStringNotEmpty("id", a.raw.Id); err != nil {
		return err
	}
	if err := validateStringNotEmpty("name", a.raw.Name); err != nil {
		return err
	}
	if err := validateURL("endpoint", a.raw.Endpoint); err != nil {
		return err
	}
	return nil
}
