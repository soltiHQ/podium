package handler

import (
	"cmp"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/uikit/htmx"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	wire "github.com/soltiHQ/control-plane/domain/wire"
	contentAgent "github.com/soltiHQ/control-plane/ui/templates/content/agent"
)

// Agents handles /api/v1/agents.
//
// Supported:
//   - GET /api/v1/agents
func (a *API) Agents(w http.ResponseWriter, r *http.Request) {
	route.Resource(w, r, routepath.ApiAgents,
		route.Endpoint{Method: http.MethodGet, Perm: enum.AgentsGet, Fn: a.agentList},
	)
}

// AgentsRouter handles /api/v1/agents/{id} and subroutes.
//
// Supported:
//   - GET    /api/v1/agents/{id}
//   - DELETE /api/v1/agents/{id}
//   - PUT    /api/v1/agents/{id}/labels
//   - POST   /api/v1/agents/{id}/credential
//   - GET    /api/v1/agents/{id}/tasks
func (a *API) AgentsRouter(w http.ResponseWriter, r *http.Request) {
	route.Router(w, r, routepath.ApiAgent,
		route.Subroute{Action: "", Method: http.MethodGet, Perm: enum.AgentsGet, Fn: a.agentDetails},
		route.Subroute{Action: "", Method: http.MethodDelete, Perm: enum.AgentsDelete, Fn: a.agentDelete},
		route.Subroute{Action: "labels", Method: http.MethodPut, Perm: enum.AgentsEdit, Fn: a.agentPatchLabels},
		route.Subroute{Action: "credential", Method: http.MethodPost, Perm: enum.AgentsReset, Fn: a.agentResetCredential},
		route.Subroute{Action: "tasks", Method: http.MethodGet, Perm: enum.AgentsGet, Fn: a.agentTasksList},
	)
}

func (a *API) agentList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	var (
		limit  = queryInt(r, "limit", 0)
		filter storage.AgentFilter

		cursor = r.URL.Query().Get("cursor")
		q      = r.URL.Query().Get("q")
	)
	if q != "" {
		filter = inmemory.NewAgentFilter().Query(q)
	}

	res, err := a.agentSVC.List(r.Context(), agent.ListQuery{
		Limit:  limit,
		Cursor: cursor,
		Filter: filter,
	})
	if err != nil {
		a.logger.Error().Err(err).Msg("agent list failed")
		response.Unavailable(w, r, mode)
		return
	}

	items := mapSlice(res.Items, wire.AgentToREST)
	response.OK(w, r, mode, &responder.View{
		Data: restv1.ListAgentsResponse{
			Items:      items,
			NextCursor: res.NextCursor,
		},
		Component: contentAgent.List(res.Items, res.NextCursor, q),
	})
}

func (a *API) agentDetails(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	ag, err := a.agentSVC.Get(r.Context(), id)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			a.logger.Error().Err(err).Str("agent_id", id).Msg("agent get failed")
		}
		response.FromError(w, r, mode, err)
		return
	}

	apiAgent := wire.AgentToREST(ag)
	response.OK(w, r, mode, &responder.View{
		Data:      apiAgent,
		Component: contentAgent.Detail(apiAgent, policy.BuildAgentDetail(a.identity(r))),
	})
}

func (a *API) agentPatchLabels(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	labels, err := decodeJSON[map[string]string](r)
	if err != nil {
		badBody(w, r, mode, err)
		return
	}

	_, err = a.agentSVC.PatchLabels(r.Context(), agent.PatchLabels{
		ID:     id,
		Labels: labels,
	})
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			a.failure(err).Str("agent_id", id).Msg("agent patch labels failed")
		}
		response.FromError(w, r, mode, err)
		return
	}

	a.logger.Info().Str("agent_id", id).Msg("agent labels updated")
	htmx.Trigger(w, event.RefreshAgents)
	a.hub.Notify(event.RefreshAgents)
	response.NoContent(w, r)
}

// agentDelete removes an agent's row and every rollout of the agent, in one
// transaction. Its credential and its execution history stay, so an agent that
// still runs registers again at its next sync, with no rollouts: reset its
// credential first to keep it out. The pooled connection to the agent is closed
// once the delete is committed, and agent_deleted records who deleted it.
func (a *API) agentDelete(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	removed, err := a.agentSVC.Delete(r.Context(), id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			response.NotFound(w, r, mode)
			return
		}
		a.failure(err).Str("agent_id", id).Msg("agent delete failed")
		response.StoreFailed(w, r, mode, err)
		return
	}

	a.proxyPool.Forget(id)
	a.logger.Info().Str("agent_id", id).Str("by", a.actor(r)).Msg("agent deleted")
	a.hub.Record(event.AgentDeleted, event.Payload{ID: id, Name: removed.Name(), By: a.actor(r)})
	a.hub.Notify(event.RefreshAgents)
	htmx.Redirect(w, routepath.PageAgents)
	response.NoContent(w, r)
}

// agentResetCredential issues the agent a new credential and answers its token,
// once: the token is in this answer and nowhere else, not in the event the reset
// records, not in a log. The old token is refused from the commit on, and the
// pooled connection to the agent is closed once the reset is committed, so that
// no connection that podium opened with the old credential outlives it. With
// agent authentication off there is nothing to reset: 409.
func (a *API) agentResetCredential(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	issued, err := a.agentSVC.ResetCredential(r.Context(), id)
	switch {
	case err == nil:
	case errors.Is(err, storage.ErrNotFound):
		response.NotFound(w, r, mode)
		return
	case errors.Is(err, agent.ErrAuthDisabled):
		response.Conflict(w, r, mode, "Agent authentication is off (agent_auth.require is false), so there is no credential to reset.")
		return
	case errors.Is(err, agent.ErrGenerationExhausted):
		response.Conflict(w, r, mode, "The credential is at its last generation and cannot be reset.")
		return
	default:
		a.failure(err).Str("agent_id", id).Msg("agent credential reset failed")
		response.StoreFailed(w, r, mode, err)
		return
	}

	a.proxyPool.Forget(id)
	logged := a.logger.Info().Str("agent_id", id).Str("by", a.actor(r))
	if issued.Derived {
		logged = logged.Uint32("generation", issued.Generation)
	}
	logged.Msg("agent credential reset")
	var name string
	if ag, err := a.agentSVC.Get(r.Context(), id); err == nil {
		name = ag.Name()
	}
	a.hub.Record(event.AgentCredentialReset, event.Payload{ID: id, Name: name, By: a.actor(r)})
	a.hub.Notify(event.RefreshDashboard)

	out := restv1.ResetAgentCredentialResponse{AgentID: issued.AgentID, Token: issued.Token}
	if issued.Derived {
		generation := issued.Generation
		out.Generation = &generation
	}
	response.OK(w, r, mode, &responder.View{Data: out})
}

// streamKeepalive is how long the log stream may stay silent before it writes
// an SSE comment. A reverse proxy closes a connection it sees no bytes on
// (nginx proxy_read_timeout defaults to 60s); EventSource ignores comments.
// It is also how often the stream re-checks access, and half the time a write
// may take. It must be positive; tests shorten it.
// The event stream keeps its own copy: internal/event streamKeepalive.
var streamKeepalive = 15 * time.Second

// sseKeepalive is the comment a silent log stream writes.
const sseKeepalive = ": keepalive\n\n"

// AgentTaskLogsStream subscribes to the (agent, task) log fanout hub and
// relays events as SSE to the UI client. The hub maintains one backing
// agent connection per (agent, task) regardless of how many UI clients
// are watching; slow readers receive a synthetic Lagged envelope and
// keep going instead of dragging the rest down.
//
// Before every write the stream sets the write deadline of its connection to
// 2×streamKeepalive ahead: the server's write timeout does not end it, but a
// client whose write blocks, that is, whose buffers are full, is dropped. On a
// quiet stream small keepalives can fit into the buffers for a long time, so a
// reader that stopped reading is not guaranteed to be dropped after
// 2×streamKeepalive. It writes a keepalive comment after streamKeepalive
// without output and, every streamKeepalive whether it wrote or not, re-checks
// the viewer's access with logAccess (the session, the user and AgentsGet, like
// the route): a failed check, or one that cannot be made, ends the stream,
// within one streamKeepalive of a revocation plus at most one blocked write (at
// most 2×streamKeepalive). EventSource then reconnects through authentication,
// which answers 401 or 403, or 503 if the store is unavailable.
//
// Frame format: `data: <protojson-StreamTaskLogsResponse>\n\n`.
func (a *API) AgentTaskLogsStream(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	taskID := r.PathValue("taskID")
	mode := httpctx.ModeFromRequest(r)

	if agentID == "" || taskID == "" {
		response.BadRequest(w, r, mode)
		return
	}

	var (
		keepalive = streamKeepalive
		rc        = http.NewResponseController(w)
	)
	// deadline gives the next write 2×keepalive: a write at least every
	// keepalive keeps a reading client connected.
	deadline := func() error { return rc.SetWriteDeadline(time.Now().Add(2 * keepalive)) }

	// A writer whose deadline cannot be set is refused: the server would cut
	// the stream at its write timeout. Refusing comes before the subscription,
	// which may open a connection to the agent.
	if err := deadline(); err != nil {
		a.logger.Error().Err(err).
			Str("agent_id", agentID).
			Str("task_id", taskID).
			Msg("agent task logs: cannot set write deadline")
		response.Internal(w, r, mode)
		return
	}

	ch, unsub, err := a.logHub.SubscribeIdentity(r.Context(), agentID, taskID, r.URL.Query().Get("task_uid"))
	if err != nil {
		a.logger.Warn().Err(err).
			Str("agent_id", agentID).
			Str("task_id", taskID).
			Msg("agent task logs: subscribe failed")
		response.Unavailable(w, r, mode)
		return
	}
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if deadline() != nil {
		return
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	var (
		marshaler = protojson.MarshalOptions{EmitUnpopulated: true}
		idle      = time.NewTicker(keepalive)
		recheck   = time.NewTicker(keepalive)
	)
	defer idle.Stop()
	defer recheck.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-recheck.C:
			if err := a.logAccess(r); err != nil {
				a.logger.Debug().Err(err).
					Str("agent_id", agentID).
					Str("task_id", taskID).
					Msg("agent task logs: access lost, stream ended")
				return
			}
			continue
		case <-idle.C:
			if deadline() != nil {
				return
			}
			if _, err := io.WriteString(w, sseKeepalive); err != nil {
				return
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, err := marshaler.Marshal(ev)
			if err != nil {
				continue
			}
			if deadline() != nil {
				return
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if _, err := w.Write(payload); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return
			}
			idle.Reset(keepalive)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// Exact slot/status filters are passed to the agent before pagination.
func (a *API) agentTasksList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, agentID string) {
	ag, err := a.agentSVC.Get(r.Context(), agentID)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			a.logger.Error().Err(err).
				Str("agent_id", agentID).
				Msg("agent tasks: agent lookup failed")
		}
		response.FromError(w, r, mode, err)
		return
	}
	if ag.Endpoint() == "" {
		a.logger.Warn().
			Str("agent_id", agentID).
			Str("endpoint_type", string(ag.EndpointType())).
			Str("api_version", ag.APIVersion().String()).
			Msg("agent tasks: agent has empty endpoint (discovery never completed or dropped it)")
		response.Unavailable(w, r, mode)
		return
	}

	var (
		filter = proxy.TaskFilter{
			Status: r.URL.Query().Get("status"),
			Limit:  queryInt(r, "limit", 0),
			Cursor: r.URL.Query().Get("cursor"),
		}
		q = r.URL.Query().Get("slot")
	)

	filter.Slot = q
	token, err := a.agentSVC.AgentToken(r.Context(), agentID)
	if err != nil {
		a.logger.Error().Err(err).Str("agent_id", agentID).Msg("agent tasks: token lookup failed")
		response.Unavailable(w, r, mode)
		return
	}

	p, err := a.proxyPool.Get(ag.ID(), ag.Endpoint(), ag.EndpointType(), ag.APIVersion(), token)
	if err != nil {
		a.logger.Error().Err(err).
			Str("agent_id", agentID).
			Str("endpoint", ag.Endpoint()).
			Msg("proxy: pool get failed")
		switch {
		case errors.Is(err, proxy.ErrUnsupportedAPIVersion),
			errors.Is(err, proxy.ErrUnsupportedEndpointType):
			response.BadRequest(w, r, mode)
		default:
			response.Unavailable(w, r, mode)
		}
		return
	}

	result, err := p.ListTasks(r.Context(), filter)
	if err != nil {
		a.logger.Warn().Err(err).
			Str("agent_id", agentID).
			Str("endpoint", ag.Endpoint()).
			Msg("proxy: ListTasks failed")
		response.FromError(w, r, mode, err)
		return
	}
	slices.SortFunc(result.Tasks, func(a, b proxyv1.Task) int {
		return cmp.Compare(enum.ParseTaskStatus(a.Status).Priority(), enum.ParseTaskStatus(b.Status).Priority())
	})
	response.OK(w, r, mode, &responder.View{
		Data:      result,
		Component: contentAgent.Tasks(agentID, result.Tasks, result.Total, q, result.NextCursor),
	})
}
