package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/soltiHQ/control-plane/domain"
	discoverv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"github.com/soltiHQ/control-plane/internal/backend"
	"github.com/soltiHQ/control-plane/internal/logctx"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/httpresponse"

	"github.com/rs/zerolog"
)

// Http implements the HTTP discovery service.
type Http struct {
	logger  zerolog.Logger
	storage storage.Storage
}

// NewHttp creates a new HTTP discovery handler.
func NewHttp(logger zerolog.Logger, storage storage.Storage) *Http {
	return &Http{
		logger: logger.With().
			Str("type", "http").
			Logger(),
		storage: storage,
	}
}

// Sync handles HTTP Sync requests from agents.
func (h *Http) Sync(w http.ResponseWriter, r *http.Request) {
	var (
		ctx    = r.Context()
		logger = logctx.From(ctx, h.logger)
	)
	if r.Method != http.MethodPost {
		logger.Warn().Str("method", r.Method).Msg("invalid method")
		if err := httpresponse.NotAllowed(ctx, w, "method not supported"); err != nil {
			logctx.Error(ctx, h.logger, err, "failed to write not-allowed response")
		}
		return
	}

	var req discoverv1.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Warn().Err(err).Msg("failed to decode sync request payload")
		if err = httpresponse.BadRequest(ctx, w, "invalid JSON payload"); err != nil {
			logctx.Error(ctx, h.logger, err, "failed to write bad-request response")
		}
		return
	}
	agent, err := domain.NewAgentModel(&req)
	if err != nil {
		logger.Warn().Err(err).Msg("invalid sync request payload")
		if err = httpresponse.BadRequest(ctx, w, "invalid agent data"); err != nil {
			logctx.Error(ctx, h.logger, err, "failed to write bad-request response")
		}
		return
	}
	if err = backend.Discovery(ctx, logger, h.storage, agent); err != nil {
		if err = httpresponse.InternalError(ctx, w, "internal error"); err != nil {
			logctx.Error(ctx, h.logger, err, "failed to write internal-error response")
		}
		return
	}
	resp := &discoverv1.SyncResponse{
		Success:  true,
		Message:  "ok",
		Metadata: map[string]string{"type": "ok"},
	}
	if err = httpresponse.OK(ctx, w, resp); err != nil {
		logctx.Error(ctx, h.logger, err, "failed to write ok response")
	}
}
