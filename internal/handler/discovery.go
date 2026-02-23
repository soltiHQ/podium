package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	discoveryv1 "github.com/soltiHQ/control-plane/api/discovery/v1"
	genv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
)

// HTTPDiscovery handles agent discovery over HTTP.
type HTTPDiscovery struct {
	logger   zerolog.Logger
	agentSVC *agent.Service
}

// NewHTTPDiscovery creates a new HTTP discovery handler.
func NewHTTPDiscovery(logger zerolog.Logger, agentSVC *agent.Service) *HTTPDiscovery {
	if agentSVC == nil {
		panic("handler.HTTPDiscovery: agentSVC is nil")
	}
	return &HTTPDiscovery{
		logger:   logger.With().Str("handler", "discovery-http").Logger(),
		agentSVC: agentSVC,
	}
}

// Sync handles POST /api/v1/discovery/sync.
func (h *HTTPDiscovery) Sync(w http.ResponseWriter, r *http.Request) {
	mode := response.ModeFromRequest(r)

	if r.Method != http.MethodPost {
		response.NotAllowed(w, r, mode)
		return
	}

	var in discoveryv1.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		h.logger.Warn().Err(err).Msg("sync: failed to decode request body")
		response.BadRequest(w, r, mode)
		return
	}

	h.logger.Debug().
		Str("agent_id", in.ID).
		Str("name", in.Name).
		Str("endpoint", in.Endpoint).
		Int("endpoint_type", in.EndpointType).
		Int("api_version", in.APIVersion).
		Msg("sync request received")

	a, err := model.NewAgentFromSync(&in)
	if err != nil {
		h.logger.Warn().Err(err).
			Str("agent_id", in.ID).
			Int("endpoint_type", in.EndpointType).
			Int("api_version", in.APIVersion).
			Msg("invalid sync request")
		response.BadRequest(w, r, mode)
		return
	}

	if err = h.agentSVC.Upsert(r.Context(), a); err != nil {
		h.logger.Error().Err(err).Str("agent_id", in.ID).Msg("upsert failed")
		response.Unavailable(w, r, mode)
		return
	}

	response.OK(w, r, mode, &responder.View{
		Data: discoveryv1.SyncResponse{Success: true},
	})
}

// GRPCDiscovery implements genv1.DiscoverServiceServer.
type GRPCDiscovery struct {
	genv1.UnimplementedDiscoverServiceServer
	logger   zerolog.Logger
	agentSVC *agent.Service
}

// NewGRPCDiscovery creates a new gRPC discovery handler.
func NewGRPCDiscovery(logger zerolog.Logger, agentSVC *agent.Service) *GRPCDiscovery {
	if agentSVC == nil {
		panic("handler.GRPCDiscovery: agentSVC is nil")
	}
	return &GRPCDiscovery{
		logger:   logger.With().Str("handler", "discovery-grpc").Logger(),
		agentSVC: agentSVC,
	}
}

// Sync implements genv1.DiscoverServiceServer.
func (g *GRPCDiscovery) Sync(ctx context.Context, req *genv1.SyncRequest) (*genv1.SyncResponse, error) {
	a, err := model.NewAgentFromProto(req)
	if err != nil {
		g.logger.Warn().Err(err).Msg("invalid sync request")
		return nil, status.Errorf(codes.InvalidArgument, "invalid agent data: %v", err)
	}

	if err = g.agentSVC.Upsert(ctx, a); err != nil {
		g.logger.Error().Err(err).Str("agent_id", req.GetId()).Msg("upsert failed")
		return nil, status.Errorf(codes.Internal, "upsert failed")
	}

	return &genv1.SyncResponse{Success: true}, nil
}
