package model

import (
	"time"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/kind"
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
	createdAt         time.Time
	updatedAt         time.Time
	lastSeenAt        time.Time
	heartbeatInterval time.Duration

	uptimeSeconds int64

	id       string
	name     string
	endpoint string

	os       string
	arch     string
	platform string

	endpointType kind.EndpointType
	apiVersion   kind.APIVersion
	status       kind.AgentStatus

	metadata map[string]string
	labels   map[string]string
}

// NewAgent creates a new agent domain entity.
func NewAgent(id, name, endpoint string) (*Agent, error) {
	if id == "" {
		return nil, domain.ErrEmptyID
	}
	now := time.Now()
	return &Agent{
		createdAt:  now,
		updatedAt:  now,
		lastSeenAt: now,

		id:       id,
		name:     name,
		endpoint: endpoint,

		metadata: make(map[string]string),
		labels:   make(map[string]string),

		status: kind.AgentStatusActive,
	}, nil
}

// AgentParams is a transport-agnostic set of fields for constructing an Agent
// from an external discovery payload (HTTP or gRPC).
type AgentParams struct {
	ID       string
	Name     string
	Endpoint string

	EndpointType int
	APIVersion   int

	OS       string
	Arch     string
	Platform string

	UptimeSeconds      int64
	HeartbeatIntervalS int

	Metadata map[string]string
}

// NewAgentFrom constructs an Agent from transport-agnostic AgentParams.
//
// Performs a defensive copy of Metadata.
func NewAgentFrom(p AgentParams) (*Agent, error) {
	if p.ID == "" {
		return nil, domain.ErrEmptyID
	}

	epType, err := kind.EndpointTypeFromInt(p.EndpointType)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	md := make(map[string]string, len(p.Metadata))
	for k, v := range p.Metadata {
		md[k] = v
	}

	return &Agent{
		createdAt: now,
		updatedAt: now,

		metadata: md,
		labels:   make(map[string]string),

		id:           p.ID,
		name:         p.Name,
		endpoint:     p.Endpoint,
		endpointType: epType,
		apiVersion:   kind.APIVersionFromInt(p.APIVersion),
		os:           p.OS,
		arch:         p.Arch,
		platform:     p.Platform,

		uptimeSeconds: p.UptimeSeconds,

		status:            kind.AgentStatusActive,
		lastSeenAt:        now,
		heartbeatInterval: time.Duration(p.HeartbeatIntervalS) * time.Second,
	}, nil
}

// ID returns the agent's unique identifier.
func (a *Agent) ID() string { return a.id }

// Name returns the agent's name.
func (a *Agent) Name() string { return a.name }

// Endpoint returns the agent's network address.
func (a *Agent) Endpoint() string { return a.endpoint }

// EndpointType returns the agent's transport protocol.
func (a *Agent) EndpointType() kind.EndpointType { return a.endpointType }

// APIVersion returns the agent's API version.
func (a *Agent) APIVersion() kind.APIVersion { return a.apiVersion }

// UptimeSeconds returns the agent-reported uptime in seconds.
func (a *Agent) UptimeSeconds() int64 { return a.uptimeSeconds }

// OS returns the agent's operating system.
func (a *Agent) OS() string { return a.os }

// Arch returns the agent's architecture.
func (a *Agent) Arch() string { return a.arch }

// Platform returns the agent's platform.
func (a *Agent) Platform() string { return a.platform }

// Status returns the agent's lifecycle status.
func (a *Agent) Status() kind.AgentStatus { return a.status }

// LastSeenAt returns the timestamp of the agent's last successful sync.
func (a *Agent) LastSeenAt() time.Time { return a.lastSeenAt }

// HeartbeatInterval returns the agent-reported heartbeat interval.
func (a *Agent) HeartbeatInterval() time.Duration { return a.heartbeatInterval }

// SetStatus updates the agent's lifecycle status.
func (a *Agent) SetStatus(s kind.AgentStatus) {
	a.status = s
	a.updatedAt = time.Now()
}

// SetHeartbeatInterval sets the agent's heartbeat interval.
func (a *Agent) SetHeartbeatInterval(d time.Duration) { a.heartbeatInterval = d }

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

		id:           a.id,
		name:         a.name,
		endpoint:     a.endpoint,
		endpointType: a.endpointType,
		apiVersion:   a.apiVersion,
		os:           a.os,
		arch:         a.arch,
		platform:     a.platform,

		uptimeSeconds: a.uptimeSeconds,

		status:            a.status,
		lastSeenAt:        a.lastSeenAt,
		heartbeatInterval: a.heartbeatInterval,
	}
}
