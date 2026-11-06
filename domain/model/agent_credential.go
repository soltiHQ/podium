package model

import (
	"time"

	"github.com/soltiHQ/control-plane/domain"
)

var _ domain.Entity[*AgentCredential] = (*AgentCredential)(nil)

// AgentTokenMaxBytes caps the bearer token discovery accepts and enrolls. The
// store copies and syncs its whole state on every write, so a few huge tokens
// would slow down or exhaust the control plane. The cap applies to tokens
// agents send, never to stored credentials: one an older version stored with
// a longer token still loads.
const AgentTokenMaxBytes = 1024

// AgentCredential is the bearer credential of one agent, which discovery checks
// on every sync and podium presents on its own calls to the agent. Keyed by
// agent ID: one credential per agent. The first sync of an unknown agent ID
// enrolls it. It outlives the agent row: deleting an agent leaves its
// credential, and nothing resets one yet.
//
// A credential is one of two kinds:
//   - derived: enrolled while an enrollment secret is set. Podium recomputes
//     the token from the secret, the agent ID and the generation, so the
//     credential holds no token, only the generation and the ID of the secret
//     it derives from;
//   - stored: enrolled without the secret, with the token the agent chose
//     (trust on first use), or by an older version, which stored every token,
//     one an earlier secret derived included. The credential holds that raw
//     token.
//
// A stored token is a raw secret. It is replicated through Raft so any replica
// can verify inbound discovery and authenticate outbound proxy calls, but it
// MUST NOT be serialized to any API/UI response or logged in clear.
type AgentCredential struct {
	createdAt time.Time
	updatedAt time.Time

	agentID string
	token   string // stored kind only

	derived    bool
	generation uint32
	secretID   []byte
}

// NewDerivedAgentCredential builds the derived credential of an agent: the
// generation of its token and secretID, the ID of the enrollment secret the
// token derives from. It holds no token.
func NewDerivedAgentCredential(agentID string, generation uint32, secretID []byte) (*AgentCredential, error) {
	if agentID == "" {
		return nil, domain.ErrEmptyID
	}
	if len(secretID) == 0 {
		return nil, domain.ErrFieldEmpty
	}

	now := time.Now()
	return &AgentCredential{
		createdAt:  now,
		updatedAt:  now,
		agentID:    agentID,
		derived:    true,
		generation: generation,
		secretID:   append([]byte(nil), secretID...),
	}, nil
}

// NewStoredAgentCredential binds a raw token to an agent that enrolled without
// the enrollment secret. It also rebuilds stored credentials, those of older
// versions included, so it accepts a token of any length; enrollment applies
// AgentTokenMaxBytes before it.
func NewStoredAgentCredential(agentID, token string) (*AgentCredential, error) {
	if agentID == "" {
		return nil, domain.ErrEmptyID
	}
	if token == "" {
		return nil, domain.ErrFieldEmpty
	}

	now := time.Now()
	return &AgentCredential{
		createdAt: now,
		updatedAt: now,
		agentID:   agentID,
		token:     token,
	}, nil
}

// ID returns the agent ID — the credential key.
func (c *AgentCredential) ID() string { return c.agentID }

// AgentID is an alias for ID for call-site clarity.
func (c *AgentCredential) AgentID() string { return c.agentID }

// Derived reports whether the credential is derived from the enrollment secret,
// and so holds no token.
func (c *AgentCredential) Derived() bool { return c.derived }

// Generation returns the generation a derived credential's token is derived
// for, 0 for a stored credential.
func (c *AgentCredential) Generation() uint32 { return c.generation }

// SecretID returns a copy of the ID of the enrollment secret a derived
// credential derives from, nil for a stored credential.
func (c *AgentCredential) SecretID() []byte {
	if c.secretID == nil {
		return nil
	}
	return append([]byte(nil), c.secretID...)
}

// Token returns the raw bearer secret of a stored credential, "" for a derived
// one.
//
// SECURITY: never serialize this outside the control plane (no API/UI exposure)
// and never log it in clear. Used only for constant-time verification and for
// authenticating outbound proxy calls to the agent.
func (c *AgentCredential) Token() string { return c.token }

// CreatedAt returns the enrollment (first-seen) timestamp.
func (c *AgentCredential) CreatedAt() time.Time { return c.createdAt }

// UpdatedAt returns the last modification timestamp.
func (c *AgentCredential) UpdatedAt() time.Time { return c.updatedAt }

// SetCreatedAt restores the creation timestamp (persistence hook).
func (c *AgentCredential) SetCreatedAt(t time.Time) { c.createdAt = t }

// SetUpdatedAt restores the modification timestamp (persistence hook).
func (c *AgentCredential) SetUpdatedAt(t time.Time) { c.updatedAt = t }

// Clone returns a deep copy.
func (c *AgentCredential) Clone() *AgentCredential {
	cp := *c
	cp.secretID = c.SecretID()
	return &cp
}
