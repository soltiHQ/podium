package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/soltiHQ/control-plane/api/v1"
	genv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service/agent"
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
	if r.Method != http.MethodPost {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	var in v1.Agent
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	a, err := model.NewAgentFromV1(&in)
	if err != nil {
		h.logger.Warn().Err(err).Str("agent_id", in.ID).Msg("invalid sync request")
		http.Error(w, "invalid agent data", http.StatusBadRequest)
		return
	}

	if err = h.agentSVC.Upsert(r.Context(), a); err != nil {
		h.logger.Error().Err(err).Str("agent_id", in.ID).Msg("upsert failed")
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v1.AgentSyncResponse{Success: true})
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
