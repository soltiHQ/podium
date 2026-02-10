package handlers

import (
	"encoding/json"
	"net/http"

	discoverv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/backend"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"

	"github.com/rs/zerolog"
)

// HTTPDiscovery handles HTTP discovery endpoints.
type HTTPDiscovery struct {
	logger  zerolog.Logger
	backend *backend.Discovery
}

func NewHTTPDiscovery(logger zerolog.Logger, backend *backend.Discovery) *HTTPDiscovery {
	return &HTTPDiscovery{
		logger:  logger.With().Str("handler", "http_discovery").Logger(),
		backend: backend,
	}
}

// Sync handles POST /api/v1/discovery/sync (example path).
func (x *HTTPDiscovery) Sync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req discoverv1.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, r, response.RenderPage) // for /api/* negotiate will pick JSON
		return
	}

	agent, err := model.NewAgentFromProto(&req)
	if err != nil {
		response.BadRequest(w, r, response.RenderPage)
		return
	}

	if err := x.backend.Sync(r.Context(), x.logger, agent); err != nil {
		response.Unavailable(w, r, response.RenderPage) // или InternalError если добавишь 500 helper
		return
	}

	response.OK(w, r, response.RenderPage, &responder.View{
		Data: &discoverv1.SyncResponse{
			Success:  true,
			Message:  "ok",
			Metadata: map[string]string{"type": "ok"},
		},
	})
}
