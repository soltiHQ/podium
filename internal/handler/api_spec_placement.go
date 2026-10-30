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

// planTokensFromIfMatch returns the plan tokens a Deploy request names in
// If-Match, read as RFC 9110 lists over every If-Match header: each strong
// entity tag, its quotes stripped; a bare tag counts as a strong one. Only an
// absent header and a "*" anywhere give nil, so Deploy runs without the
// placement check, as before. A weak tag never matches (If-Match compares
// strongly) and is left out, like an empty list element and a tag whose quote
// is not closed: a header that names no strong tag, a blank one for instance,
// gives an empty list, which no plan matches.
func planTokensFromIfMatch(r *http.Request) []string {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		return nil
	}
	tokens := []string{}
	for _, value := range values {
		for rest := value; ; {
			rest = strings.TrimLeft(rest, " \t,")
			if rest == "" {
				break
			}
			weak := strings.HasPrefix(rest, "W/")
			if weak {
				rest = rest[len("W/"):]
			}
			var tag string
			if quoted, ok := strings.CutPrefix(rest, `"`); ok {
				end := strings.IndexByte(quoted, '"')
				if end < 0 {
					break
				}
				tag, rest = quoted[:end], quoted[end+1:]
				if i := strings.IndexByte(rest, ','); i >= 0 {
					rest = rest[i:]
				} else {
					rest = ""
				}
			} else {
				end := strings.IndexByte(rest, ',')
				if end < 0 {
					end = len(rest)
				}
				tag, rest = strings.TrimSpace(rest[:end]), rest[end:]
				if tag == "*" && !weak {
					return nil
				}
			}
			if !weak {
				tokens = append(tokens, tag)
			}
		}
	}
	return tokens
}
