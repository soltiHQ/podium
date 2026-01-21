package handlers

import (
	"context"

	"github.com/soltiHQ/control-plane/domain"
	discoverv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"github.com/soltiHQ/control-plane/internal/backend"
	"github.com/soltiHQ/control-plane/internal/logctx"
	"github.com/soltiHQ/control-plane/internal/storage"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Grpc implements the gRPC discovery service.
type Grpc struct {
	discoverv1.UnimplementedDiscoverServiceServer

	logger  zerolog.Logger
	storage storage.Storage
}

// NewGrpc creates a new gRPC discovery handler.
func NewGrpc(logger zerolog.Logger, storage storage.Storage) *Grpc {
	return &Grpc{
		logger: logger.With().
			Str("type", "grpc").
			Logger(),
		storage: storage,
	}
}

// Sync handles gRPC Sync requests from agents.
func (g *Grpc) Sync(ctx context.Context, req *discoverv1.SyncRequest) (*discoverv1.SyncResponse, error) {
	logger := logctx.From(ctx, g.logger)

	agent, err := domain.NewAgentModel(req)
	if err != nil {
		logger.Warn().Err(err).Msg("invalid sync request payload")
		return nil, status.Error(codes.Internal, "invalid agent data")
	}
	if err = backend.Discovery(ctx, logger, g.storage, agent); err != nil {
		return nil, status.Error(codes.Internal, "internal error")
	}
	return &discoverv1.SyncResponse{
		Success:  true,
		Message:  "ok",
		Metadata: map[string]string{"type": "ok"},
	}, nil
}
