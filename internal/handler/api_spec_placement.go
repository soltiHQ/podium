package handler

import (
	"net/http"
	"strings"

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
// The ETag is the plan token, which Deploy takes as If-Match.
func (a *API) specPlacement(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	route.Guard(w, r, enum.AgentsGet, func(w http.ResponseWriter, r *http.Request) {
		plan, err := a.specSVC.PreviewPlacement(r.Context(), id)
		if err != nil {
			response.FromError(w, r, mode, err)
			return
		}
		dto := wire.PlacementToREST(plan)
		w.Header().Set("ETag", `"`+plan.Token+`"`)
		response.OK(w, r, mode, &responder.View{Data: dto, Component: contentSpec.Placement(dto)})
	})
}

// planTokenFromIfMatch returns the plan token a Deploy request names in
// If-Match: one strong entity tag, its quotes stripped. An absent header and
// "*" give "", so Deploy runs without the placement check, as before. An empty
// tag ("") keeps its quotes: it names no plan, so Deploy refuses it.
func planTokenFromIfMatch(r *http.Request) string {
	tag := strings.TrimSpace(r.Header.Get("If-Match"))
	if tag == "*" {
		return ""
	}
	if len(tag) > 2 && tag[0] == '"' && tag[len(tag)-1] == '"' {
		return tag[1 : len(tag)-1]
	}
	return tag
}
