package handlers

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/backend"
	"github.com/soltiHQ/control-plane/internal/logctx"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/response"

	"github.com/rs/zerolog"
)

// Http implements the HTTP api service.
type Http struct {
	logger  zerolog.Logger
	storage storage.Storage
}

// NewHttp creates a new HTTP api handler.
func NewHttp(logger zerolog.Logger, storage storage.Storage) *Http {
	return &Http{
		logger: logger.With().
			Str("type", "http").
			Logger(),
		storage: storage,
	}
}

// AgentList handles HTTP agent list request.
func (h *Http) AgentList(w http.ResponseWriter, r *http.Request) {
	var (
		ctx    = r.Context()
		logger = logctx.From(ctx, h.logger)
	)

	if r.Method != http.MethodGet {
		_ = response.NotAllowed(ctx, w, "method not supported")
		return
	}

	agents, err := backend.AgentList(ctx, logger, h.storage)
	if err != nil {
		logger.Error().Err(err).Msg("agent list failed")
		response.FromError(ctx, w, err)
		return
	}
	_ = response.OK(ctx, w, agents)
}
