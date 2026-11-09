package agent

import (
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

const defaultListLimit = 30

// ListQuery describes a paginated agents listing request.
type ListQuery struct {
	// Filter is a storage-level filter.
	// Backends validate that the filter was constructed for that backend and return storage.ErrInvalidArgument otherwise.
	Filter storage.AgentFilter

	Cursor string
	Limit  int
}

// Page is a paginated agents listing result.
type Page struct {
	Items      []*model.Agent
	NextCursor string
}

// PatchLabels updates control-plane owned labels for an agent.
//
// Semantics:
//   - ID is required.
//   - Labels replace the entire label set (no merge).
type PatchLabels struct {
	Labels map[string]string
	ID     string
}

// UpsertResult says what an Upsert changed in the agent's row.
type UpsertResult struct {
	// Visible is true when the agent is new, or its status, name, endpoint,
	// labels or capabilities differ from the stored row: what operators watch
	// and a sync can change (model.Agent.ChangedVisiblySince). The time of the
	// sync and the uptime move with every sync and are not visible, so a
	// heartbeat that changes nothing else reports false.
	Visible bool

	// Reconnected is true when the agent syncs again after being away: it is
	// new to the store, its row was not active (inactive or disconnected, so it
	// came back), or it reports less uptime than the row (it restarted between
	// two syncs). It is model.Agent.ReconnectedSince, the rule that re-arms
	// failed rollouts and ends the sync runner's backoff.
	Reconnected bool
}

// IssuedCredential is the credential ResetCredential issued, with its token.
type IssuedCredential struct {
	AgentID string
	// Token is the new token of the agent: the only place podium returns it,
	// apart from the calls it makes with it.
	Token string
	// Derived says whether the credential is derived from the enrollment secret,
	// which is so while one is set. Then Generation is its generation, at least 1,
	// and `podium agent-token --generation <Generation> <AgentID>` prints Token
	// again. A stored credential has a random token and no generation, 0 here.
	Derived    bool
	Generation uint32
}
