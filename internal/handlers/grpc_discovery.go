package handlers

import (
	"context"

	discoverv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/backend"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCDiscovery implements the gRPC discovery service.
type GRPCDiscovery struct {
	discoverv1.UnimplementedDiscoverServiceServer

	logger  zerolog.Logger
	backend *backend.Discovery
}

func NewGRPCDiscovery(logger zerolog.Logger, backend *backend.Discovery) *GRPCDiscovery {
	return &GRPCDiscovery{
		logger:  logger.With().Str("handler", "grpc_discovery").Logger(),
		backend: backend,
	}
}

func (g *GRPCDiscovery) Sync(ctx context.Context, req *discoverv1.SyncRequest) (*discoverv1.SyncResponse, error) {
	agent, err := model.NewAgentFromProto(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid agent data")
	}

	if err := g.backend.Sync(ctx, g.logger, agent); err != nil {
		return nil, status.Error(codes.Internal, "internal error")
	}

	return &discoverv1.SyncResponse{
		Success:  true,
		Message:  "ok",
		Metadata: map[string]string{"type": "ok"},
	}, nil
}
