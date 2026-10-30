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
//   - GET  /api/v1/agents/{id}
//   - PUT  /api/v1/agents/{id}/labels
//   - GET  /api/v1/agents/{id}/tasks
func (a *API) AgentsRouter(w http.ResponseWriter, r *http.Request) {
	route.Router(w, r, routepath.ApiAgent,
		route.Subroute{Action: "", Method: http.MethodGet, Perm: enum.AgentsGet, Fn: a.agentDetails},
		route.Subroute{Action: "labels", Method: http.MethodPut, Perm: enum.AgentsEdit, Fn: a.agentPatchLabels},
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
			a.logger.Error().Err(err).Str("agent_id", id).Msg("agent patch labels failed")
		}
		response.FromError(w, r, mode, err)
		return
	}

	a.logger.Info().Str("agent_id", id).Msg("agent labels updated")
	htmx.Trigger(w, event.RefreshAgents)
	a.hub.Notify(event.RefreshAgents)
	response.NoContent(w, r)
}

// streamKeepalive is how long the log stream may stay silent before it writes
// an SSE comment. A reverse proxy closes a connection it sees no bytes on
// (nginx proxy_read_timeout defaults to 60s); EventSource ignores comments.
// It must be positive; tests shorten it.
var streamKeepalive = 15 * time.Second

// sseKeepalive is the comment a silent log stream writes.
const sseKeepalive = ": keepalive\n\n"

// AgentTaskLogsStream subscribes to the (agent, task) log fanout hub and
// relays events as SSE to the UI client. The hub maintains one backing
// agent connection per (agent, task) regardless of how many UI clients
// are watching; slow readers receive a synthetic Lagged envelope and
// keep going instead of dragging the rest down.
//
// The stream clears the write deadline of its connection, so the server's
// write timeout does not end it, and writes a keepalive comment after
// streamKeepalive without output.
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

	// A writer that cannot clear the deadline is refused: the server would cut
	// the stream at its write timeout. Refusing comes before the subscription,
	// which may open a connection to the agent.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		a.logger.Error().Err(err).
			Str("agent_id", agentID).
			Str("task_id", taskID).
			Msg("agent task logs: cannot disable write deadline")
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
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	var (
		marshaler = protojson.MarshalOptions{EmitUnpopulated: true}
		keepalive = streamKeepalive
		idle      = time.NewTicker(keepalive)
	)
	defer idle.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-idle.C:
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

	p, err := a.proxyPool.Get(ag.Endpoint(), ag.EndpointType(), ag.APIVersion(), token)
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
