package handler

import (
	"net/http"
	"net/url"
	"strconv"
	"unicode/utf8"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/service/history"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
	contentHistory "github.com/soltiHQ/control-plane/ui/templates/content/history"
)

const (
	historyDefaultLimit = 50
	historyMaxLimit     = 100
)

// History exposes stored observations without consulting live agents or specs.
func (a *API) History(w http.ResponseWriter, r *http.Request) {
	route.Resource(w, r, routepath.ApiHistory, route.Endpoint{Method: http.MethodGet, Perm: enum.AgentsGet, Fn: a.historyList})
}

func (a *API) historyList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	query, err := historyQuery(r)
	if err != nil {
		response.BadRequestMsg(w, r, mode, "Invalid history filters, cursor or page size.")
		return
	}
	criteria := storage.ExecutionQueryCriteria{AgentID: query.AgentID, SpecID: query.SpecID, TaskName: query.TaskName, TaskUID: query.TaskUID}
	if query.Status != "" {
		criteria.Statuses = []string{query.Status}
	}
	page, err := a.historySVC.List(r.Context(), history.ListQuery{Limit: query.Limit, Cursor: query.Cursor, Criteria: criteria})
	if err != nil {
		response.FromError(w, r, mode, err)
		return
	}
	items := make([]restv1.ExecutionRecord, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, wire.ExecutionToREST(item))
	}
	dto := restv1.ListHistoryResponse{Items: items, NextCursor: page.NextCursor, Source: "agent-poll", CaptureComplete: false}
	response.OK(w, r, mode, &responder.View{Data: dto, Component: contentHistory.Results(dto, query)})
}

func historyQuery(r *http.Request) (restv1.HistoryQuery, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return restv1.HistoryQuery{}, storage.ErrInvalidArgument
	}
	query := restv1.HistoryQuery{AgentID: values.Get("agent_id"), SpecID: values.Get("spec_id"), TaskName: values.Get("task_name"), TaskUID: values.Get("task_uid"), Status: values.Get("status"), Cursor: values.Get("cursor"), Limit: historyDefaultLimit}
	for _, key := range []string{"agent_id", "spec_id", "task_name", "task_uid", "status", "cursor", "limit"} {
		if len(values[key]) > 1 {
			return query, storage.ErrInvalidArgument
		}
	}
	for _, value := range []string{query.AgentID, query.SpecID, query.TaskName, query.TaskUID} {
		if len(value) > model.ExecutionMetadataMaxBytes || !utf8.ValidString(value) {
			return query, storage.ErrInvalidArgument
		}
	}
	if query.Status != "" && !model.IsTerminalExecutionStatus(query.Status) {
		return query, storage.ErrInvalidArgument
	}
	if len(query.Cursor) > 2048 || !utf8.ValidString(query.Cursor) {
		return query, storage.ErrInvalidArgument
	}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			return query, storage.ErrInvalidArgument
		}
		query.Limit = min(limit, historyMaxLimit)
	}
	return query, nil
}
