package model

import (
	"time"

	v1 "github.com/soltiHQ/control-plane/api/v1"
	"github.com/soltiHQ/control-plane/domain"
	genv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
)

var _ domain.Entity[*Agent] = (*Agent)(nil)

// Agent is a core domain entity that represents an Agent connected to the control-plane.
//
// Agent is a remote worker/node that runs an agent process and periodically reports its state to the control-plane.
//
// Notes:
//   - Metadata is agent-owned data reported by the agent (not modified).
//   - Labels are control-plane owned annotations (operators/system), not reported by the agent.
type Agent struct {
	createdAt time.Time
	updatedAt time.Time

	metadata map[string]string
	labels   map[string]string

	id       string
	name     string
	endpoint string
	os       string
	arch     string
	platform string

	uptimeSeconds int64
}

// NewAgent creates a new agent domain entity.
func NewAgent(id, name, endpoint string) (*Agent, error) {
	if id == "" {
		return nil, domain.ErrEmptyID
	}
	now := time.Now()
	return &Agent{
		createdAt: now,
		updatedAt: now,

		id:       id,
		name:     name,
		endpoint: endpoint,

		metadata: make(map[string]string),
		labels:   make(map[string]string),
	}, nil
}

// NewAgentFromV1 constructs an Agent from an HTTP API DTO.
//
// This method performs defensive copies of maps and does NOT keep references to the input.
func NewAgentFromV1(in *v1.Agent) (*Agent, error) {
	if in == nil {
		return nil, domain.ErrNilSyncRequest
	}
	if in.ID == "" {
		return nil, domain.ErrEmptyID
	}

	var (
		now = time.Now()
		md  = make(map[string]string, len(in.Metadata))
	)
	for k, v := range in.Metadata {
		md[k] = v
	}
	return &Agent{
		createdAt: now,
		updatedAt: now,

		metadata: md,
		labels:   make(map[string]string),

		id:       in.ID,
		name:     in.Name,
		endpoint: in.Endpoint,
		os:       in.OS,
		arch:     in.Arch,
		platform: in.Platform,

		uptimeSeconds: in.UptimeSeconds,
	}, nil
}

// NewAgentFromProto constructs an Agent from a SyncRequest.
//
// This method performs defensive copies of maps and does NOT keep references to the proto object.
func NewAgentFromProto(req *genv1.SyncRequest) (*Agent, error) {
	if req == nil {
		return nil, domain.ErrNilSyncRequest
	}
	if req.Id == "" {
		return nil, domain.ErrEmptyID
	}

	var (
		now = time.Now()
		md  = make(map[string]string, len(req.Metadata))
	)
	for k, v := range req.Metadata {
		md[k] = v
	}
	return &Agent{
		createdAt: now,
		updatedAt: now,

		metadata: md,
		labels:   make(map[string]string),

		id:       req.Id,
		name:     req.Name,
		endpoint: req.Endpoint,
		os:       req.Os,
		arch:     req.Arch,
		platform: req.Platform,

		uptimeSeconds: req.UptimeSeconds,
	}, nil
}

// ID returns the agent's unique identifier.
func (a *Agent) ID() string { return a.id }

// Name returns the agent's name.
func (a *Agent) Name() string { return a.name }

// Endpoint returns the agent's network address.
func (a *Agent) Endpoint() string { return a.endpoint }

// UptimeSeconds returns the agent-reported uptime in seconds.
func (a *Agent) UptimeSeconds() int64 { return a.uptimeSeconds }

// OS returns the agent's operating system.
func (a *Agent) OS() string { return a.os }

// Arch returns the agent's architecture.
func (a *Agent) Arch() string { return a.arch }

// Platform returns the agent's platform.
func (a *Agent) Platform() string { return a.platform }

// CreatedAt returns the creation timestamp.
func (a *Agent) CreatedAt() time.Time { return a.createdAt }

// SetCreatedAt overrides the creation timestamp (used to preserve the original value during sync).
func (a *Agent) SetCreatedAt(t time.Time) { a.createdAt = t }

// UpdatedAt returns the last modification timestamp.
func (a *Agent) UpdatedAt() time.Time { return a.updatedAt }

// Metadata returns the metadata value for the given key.
func (a *Agent) Metadata(key string) (string, bool) {
	v, ok := a.metadata[key]
	return v, ok
}

// MetadataAll returns a copy of the agent's metadata.
func (a *Agent) MetadataAll() map[string]string {
	out := make(map[string]string, len(a.metadata))
	for k, v := range a.metadata {
		out[k] = v
	}
	return out
}

// Label returns a label value for the given key.
func (a *Agent) Label(key string) (string, bool) {
	v, ok := a.labels[key]
	return v, ok
}

// LabelsAll returns a copy of the agent's labels.
func (a *Agent) LabelsAll() map[string]string {
	out := make(map[string]string, len(a.labels))
	for k, v := range a.labels {
		out[k] = v
	}
	return out
}

// LabelAdd sets a control-plane owned label on the agent.
func (a *Agent) LabelAdd(key, value string) {
	a.labels[key] = value
	a.updatedAt = time.Now()
}

// LabelDelete removes a control-plane owned label from the agent.
func (a *Agent) LabelDelete(key string) {
	delete(a.labels, key)
	a.updatedAt = time.Now()
}

// Clone creates a deep copy of the agent model.
func (a *Agent) Clone() *Agent {
	var (
		md     = make(map[string]string, len(a.metadata))
		labels = make(map[string]string, len(a.labels))
	)
	for k, v := range a.metadata {
		md[k] = v
	}
	for k, v := range a.labels {
		labels[k] = v
	}

	return &Agent{
		createdAt: a.createdAt,
		updatedAt: a.updatedAt,

		metadata: md,
		labels:   labels,

		id:       a.id,
		name:     a.name,
		endpoint: a.endpoint,
		os:       a.os,
		arch:     a.arch,
		platform: a.platform,

		uptimeSeconds: a.uptimeSeconds,
	}
}
