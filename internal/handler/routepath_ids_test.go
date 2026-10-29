package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a-h/templ"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/transportctx"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
)

// routepath escapes IDs for the browser; the router and the page handlers split
// the decoded path, so every ID must come back unchanged.
func TestRoutepathIDsReachHandlersDecoded(t *testing.T) {
	ctx := transportctx.WithIdentity(context.Background(), &identity.Identity{
		UserID:      "actor",
		Permissions: []enum.Permission{enum.AgentsGet, enum.AgentsEdit},
	})
	for _, id := range []string{"plain", "with space", `quote'"`, "100%25", "q?x#y", "<tag>&;,+=", "é🌍"} {
		var routed string
		r := httptest.NewRequest(http.MethodPut, routepath.ApiAgentLabels(id), nil).WithContext(ctx)
		route.Router(httptest.NewRecorder(), r, routepath.ApiAgent, route.Subroute{
			Action: "labels", Method: http.MethodPut, Perm: enum.AgentsEdit,
			Fn: func(_ http.ResponseWriter, _ *http.Request, _ httpctx.RenderMode, got string) { routed = got },
		})
		if routed != id {
			t.Errorf("API router: %q reached the handler as %q", id, routed)
		}

		var paged string
		r = httptest.NewRequest(http.MethodGet, routepath.PageAgentInfoByID(id), nil).WithContext(ctx)
		(&UI{}).pageParam(httptest.NewRecorder(), r, http.MethodGet, routepath.PageAgentInfo, func(_ policy.Nav, got string) templ.Component {
			paged = got
			return templ.NopComponent
		})
		if paged != id {
			t.Errorf("page handler: %q reached the template as %q", id, paged)
		}
	}
}
