package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/uikit/htmx"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
	contentAgent "github.com/soltiHQ/control-plane/ui/templates/content/agent"
)

// AgentTaskDetail returns current execution state and bounded run history.
// Agent transport errors retain the viewer and are retryable from the UI.
func (a *API) AgentTaskDetail(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	agentID, taskID := r.PathValue("id"), r.PathValue("taskID")
	ag, err := a.agentSVC.Get(r.Context(), agentID)
	if err != nil {
		response.FromError(w, r, mode, err)
		return
	}
	token, err := a.agentSVC.AgentToken(r.Context(), agentID)
	if err != nil {
		response.FromError(w, r, mode, err)
		return
	}
	p, err := a.proxyPool.Get(ag.Endpoint(), ag.EndpointType(), ag.APIVersion(), token)
	if err != nil {
		response.FromError(w, r, mode, err)
		return
	}
	task, err := p.GetTask(r.Context(), taskID)
	if err != nil {
		response.FromError(w, r, mode, err)
		return
	}
	if task == nil || task.Task == nil {
		response.NotFound(w, r, mode)
		return
	}
	runs, err := p.ListTaskRuns(r.Context(), taskID)
	if err != nil {
		response.FromError(w, r, mode, err)
		return
	}
	if runs == nil {
		response.Unavailable(w, r, mode)
		return
	}
	if runs.TaskUID != "" && runs.TaskUID != task.Task.UID {
		response.Conflict(w, r, mode, "Task was recreated while loading history. Retry to inspect the current incarnation.")
		return
	}
	response.OK(w, r, mode, &responder.View{Data: struct {
		Task any `json:"task"`
		Runs any `json:"runs"`
	}{task.Task, runs.Runs}, Component: contentAgent.TaskExecution(agentID, *task.Task, runs.Runs, policy.BuildAgentDetail(a.identity(r)))})
}

// AgentTaskCancel retains task history while requesting logical cancellation.
func (a *API) AgentTaskCancel(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	agentID, taskID := r.PathValue("id"), r.PathValue("taskID")
	ag, err := a.agentSVC.Get(ctx, agentID)
	if err != nil {
		response.FromError(w, r, mode, err)
		return
	}
	token, err := a.agentSVC.AgentToken(ctx, agentID)
	if err != nil {
		response.FromError(w, r, mode, err)
		return
	}
	p, err := a.proxyPool.Get(ag.Endpoint(), ag.EndpointType(), ag.APIVersion(), token)
	if err != nil {
		response.FromError(w, r, mode, err)
		return
	}
	c, ok := p.(proxy.TaskCanceler)
	if !ok {
		response.BadRequestMsg(w, r, mode, "This agent transport does not support cancellation.")
		return
	}
	if err = c.CancelTask(ctx, taskID); err != nil {
		response.FromError(w, r, mode, err)
		return
	}
	htmx.Trigger(w, event.RefreshAgents)
	a.hub.Notify(event.RefreshAgents)
	response.NoContent(w, r)
}
