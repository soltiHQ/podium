package handler

import (
	"net/http"

	"github.com/a-h/templ"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
	pageHistory "github.com/soltiHQ/control-plane/ui/templates/page/history"
)

func (u *UI) History(w http.ResponseWriter, r *http.Request) {
	route.Guard(w, r, enum.AgentsGet, func(w http.ResponseWriter, r *http.Request) {
		query, err := historyQuery(r)
		if err != nil {
			response.BadRequestMsg(w, r, httpctx.ModeFromRequest(r), "Invalid history filters, cursor or page size.")
			return
		}
		u.page(w, r, http.MethodGet, routepath.PageHistory, func(nav policy.Nav) templ.Component { return pageHistory.History(nav, query) })
	})
}
