// Package logctx provides helpers for enriching zerolog loggers with contextual metadata extracted from context.Context.
package logctx

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/transport/transportctx"
)

// From returns a logger enriched with context-derived fields.
func From(ctx context.Context, base zerolog.Logger) zerolog.Logger {
	log := base

	if reqID, ok := transportctx.RequestID(ctx); ok {
		log = log.With().Str("request_id", reqID).Logger()
	}
	return log
}

// Error logs an error with a context-enriched logger.
func Error(ctx context.Context, base zerolog.Logger, err error, msg string) {
	if err == nil {
		return
	}
	log := From(ctx, base)
	log.Err(err).Msg(msg)
}
