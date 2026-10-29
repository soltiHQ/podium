package handler

import (
	"net/http"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	contentSpec "github.com/soltiHQ/control-plane/ui/templates/content/spec"
)

// specPlacement also requires AgentsGet because it exposes agent inventory and
// heartbeat observations. The surrounding SpecsRouter requires SpecsGet.
func (a *API) specPlacement(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	route.Guard(w, r, enum.AgentsGet, func(w http.ResponseWriter, r *http.Request) {
		plan, err := a.specSVC.PreviewPlacement(r.Context(), id)
		if err != nil {
			response.FromError(w, r, mode, err)
			return
		}
		dto := wire.PlacementToREST(plan)
		response.OK(w, r, mode, &responder.View{Data: dto, Component: contentSpec.Placement(dto)})
	})
}
