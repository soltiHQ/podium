package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/soltiHQ/control-plane/internal/transport/grpc/status"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/clientip"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

// discoverySyncUnmarshal is a protojson.UnmarshalOptions with
// DiscardUnknown=true so that forward-compatible extensions of SyncRequest
// from newer agents don't hard-fail old control-planes.
var discoverySyncUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// discoverySyncMarshal keeps the canonical proto-JSON contract: camelCase
// field names and enum-as-string, symmetric with the SDK's pbjson output.
var discoverySyncMarshal = protojson.MarshalOptions{
	UseProtoNames:   false,
	EmitUnpopulated: false,
}

// MaxSyncBytes caps a SyncRequest to defend against runaway clients: the HTTP
// request body and the gRPC message alike.
const MaxSyncBytes = 256 << 10

// HTTPDiscovery handles agent discovery over HTTP.
type HTTPDiscovery struct {
	logger      zerolog.Logger
	agentSVC    *agent.Service
	eventHub    *event.Hub
	limiter     *ratelimit.Limiter
	resolver    *clientip.Resolver
	requireAuth bool
}

// NewHTTPDiscovery creates a new HTTP discovery handler.
//
// requireAuth turns on per-agent bearer-token authentication on every sync:
// the first sync of an unknown agent ID enrolls its token (trust on first
// use, or only the token derived from the agent service's enrollment secret
// when it has one), later ones must present it (constant-time verify). limiter
// bounds the token checks of each (client IP, agent ID) pair, see
// verifyAgentToken; resolver gives the client IP (nil trusts no proxy).
func NewHTTPDiscovery(logger zerolog.Logger, agentSVC *agent.Service, eventHub *event.Hub, limiter *ratelimit.Limiter, resolver *clientip.Resolver, requireAuth bool) *HTTPDiscovery {
	if agentSVC == nil {
		panic(service.ErrNilService)
	}
	if eventHub == nil {
		panic(event.ErrNilHub)
	}
	if limiter == nil {
		panic("handler.NewHTTPDiscovery: nil rate limiter")
	}
	return &HTTPDiscovery{
		logger:      logger.With().Str("handler", "discovery-http").Logger(),
		agentSVC:    agentSVC,
		eventHub:    eventHub,
		limiter:     limiter,
		resolver:    resolver,
		requireAuth: requireAuth,
	}
}

// Sync handles POST /api/v1/discovery/sync.
//
// The request body is expected to be canonical proto-JSON (camelCase +
// enum-as-string) matching solti.discover.v1.SyncRequest. The SDK emits
// this format via pbjson; both HTTP and gRPC paths therefore share a single
// wire schema.
func (h *HTTPDiscovery) Sync(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)

	if r.Method != http.MethodPost {
		response.NotAllowed(w, r, mode)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxSyncBytes+1))
	if err != nil {
		response.BadRequest(w, r, mode)
		return
	}

	if len(body) > MaxSyncBytes {
		http.Error(w, "discovery request too large", http.StatusRequestEntityTooLarge)
		return
	}

	var in discoverv1.SyncRequest
	if err := discoverySyncUnmarshal.Unmarshal(body, &in); err != nil {
		response.BadRequest(w, r, mode)
		return
	}

	a, err := agentFromSyncRequest(&in)
	if err != nil {
		response.BadRequestMsg(w, r, mode, "Invalid agent data: "+brokenRule(err)+".")
		return
	}

	if h.requireAuth {
		token := bearerToken(r.Header.Get("Authorization"))
		if err := verifyAgentToken(r.Context(), h.logger, h.agentSVC, h.limiter, h.resolver.IP(r), in.GetId(), token); err != nil {
			switch {
			case errors.Is(err, agent.ErrTokenTooLong):
				response.BadRequestMsg(w, r, mode, "Agent token is too long.")
			case errors.Is(err, auth.ErrRateLimited):
				response.AuthRateLimit(w, r, mode)
			case errors.Is(err, agent.ErrUnauthenticated):
				response.Unauthorized(w, r, mode)
			default:
				h.logger.Error().Err(err).Str("agent_id", in.GetId()).Msg("token verify failed")
				response.Unavailable(w, r, mode)
			}
			return
		}
	}

	existing, getErr := h.agentSVC.Get(r.Context(), in.GetId())
	if err = h.agentSVC.Upsert(r.Context(), a); err != nil {
		h.logger.Error().Err(err).Str("agent_id", in.GetId()).Msg("upsert failed")
		response.Unavailable(w, r, mode)
		return
	}
	recordSyncEvents(h.eventHub, in.GetId(), in.GetName(), existing, getErr)

	resp := &discoverv1.SyncResponse{Success: true}
	respBytes, err := discoverySyncMarshal.Marshal(resp)
	if err != nil {
		h.logger.Error().Err(err).Msg("marshal SyncResponse")
		response.Unavailable(w, r, mode)
		return
	}
	response.OK(w, r, mode, &responder.View{RawJSON: respBytes})
}

// GRPCDiscovery implements discoverv1.DiscoverServiceServer.
type GRPCDiscovery struct {
	discoverv1.UnimplementedDiscoverServiceServer
	logger      zerolog.Logger
	agentSVC    *agent.Service
	hub         *event.Hub
	limiter     *ratelimit.Limiter
	requireAuth bool
}

// NewGRPCDiscovery creates a new gRPC discovery handler.
//
// requireAuth turns on per-agent bearer-token authentication on every sync,
// as for NewHTTPDiscovery. limiter bounds the token checks of each (peer IP,
// agent ID) pair, see verifyAgentToken.
func NewGRPCDiscovery(logger zerolog.Logger, agentSVC *agent.Service, hub *event.Hub, limiter *ratelimit.Limiter, requireAuth bool) *GRPCDiscovery {
	if agentSVC == nil {
		panic(service.ErrNilService)
	}
	if hub == nil {
		panic(event.ErrNilHub)
	}
	if limiter == nil {
		panic("handler.NewGRPCDiscovery: nil rate limiter")
	}
	return &GRPCDiscovery{
		logger:      logger.With().Str("handler", "discovery-grpc").Logger(),
		agentSVC:    agentSVC,
		hub:         hub,
		limiter:     limiter,
		requireAuth: requireAuth,
	}
}

// Sync implements discoverv1.DiscoverServiceServer.
func (g *GRPCDiscovery) Sync(ctx context.Context, req *discoverv1.SyncRequest) (*discoverv1.SyncResponse, error) {
	a, err := agentFromSyncRequest(req)
	if err != nil {
		return nil, status.Errorf(ctx, codes.InvalidArgument, "invalid agent data: %s", brokenRule(err))
	}

	if g.requireAuth {
		if err := verifyAgentToken(ctx, g.logger, g.agentSVC, g.limiter, grpcPeerIP(ctx), req.GetId(), grpcBearerToken(ctx)); err != nil {
			switch {
			case errors.Is(err, agent.ErrTokenTooLong):
				return nil, status.Errorf(ctx, codes.InvalidArgument, "agent token is too long")
			case errors.Is(err, auth.ErrRateLimited):
				return nil, status.Errorf(ctx, codes.ResourceExhausted, "rate limited")
			case errors.Is(err, agent.ErrUnauthenticated):
				return nil, status.Errorf(ctx, codes.Unauthenticated, "agent authentication failed")
			default:
				g.logger.Error().Err(err).Str("agent_id", req.GetId()).Msg("token verify failed")
				return nil, status.FromError(ctx, err).Err()
			}
		}
	}

	existing, getErr := g.agentSVC.Get(ctx, req.GetId())
	if err = g.agentSVC.Upsert(ctx, a); err != nil {
		g.logger.Error().Err(err).Str("agent_id", req.GetId()).Msg("upsert failed")
		return nil, status.FromError(ctx, err).Err()
	}
	recordSyncEvents(g.hub, req.GetId(), req.GetName(), existing, getErr)
	return &discoverv1.SyncResponse{Success: true}, nil
}

// verifyAgentToken checks the bearer token of agentID synced from clientIP, for
// both Sync transports. A token over model.AgentTokenMaxBytes returns
// agent.ErrTokenTooLong before anything else. Otherwise it reserves an attempt
// on the (client IP, agent ID) pair, so checks in flight count like failures:
// it returns auth.ErrRateLimited while the failures plus the checks in flight
// of the pair reach the limit. A missing or wrong token
// (agent.ErrUnauthenticated, which includes a first-contact token other than
// the one derived from the enrollment secret) becomes a failure, and a missing
// one logs a WARN, at most rate_attempts per window for the pair; every other
// outcome, a panic included, releases the attempt.
func verifyAgentToken(ctx context.Context, logger zerolog.Logger, svc *agent.Service, limiter *ratelimit.Limiter, clientIP, agentID, token string) error {
	if len(token) > model.AgentTokenMaxBytes {
		return agent.ErrTokenTooLong
	}
	attempt, err := limiter.Reserve(time.Now(), ratelimit.AgentKey(clientIP, agentID))
	if err != nil {
		return err
	}
	defer attempt.Release() // a no-op once the attempt has failed

	if token == "" {
		// The SDK sends no token over plaintext unless told to.
		logger.Warn().Str("agent_id", agentID).
			Msg("agent sent no token: serve discovery over TLS so the SDK sends it, call allow_insecure_token_transport() in the SDK, or set agent_auth.require=false")
	}
	err = svc.VerifyOrEnrollToken(ctx, agentID, token)
	if errors.Is(err, agent.ErrUnauthenticated) {
		attempt.Fail(time.Now())
	}
	return err
}

// brokenRule is the domain rule err reports, without its sentinel prefix, for
// the answer to an agent whose sync data breaks it.
func brokenRule(err error) string {
	return strings.TrimPrefix(err.Error(), domain.ErrInvalidArgument.Error()+": ")
}

// agentFromSyncRequest builds a domain Agent from a discovery heartbeat. Shared
// by the HTTP and gRPC Sync handlers — both carry the same proto SyncRequest.
// Both call it before token enrollment, so an invalid payload enrolls nothing.
func agentFromSyncRequest(req *discoverv1.SyncRequest) (*model.Agent, error) {
	return model.NewAgentFrom(model.AgentParams{
		ID:                 req.GetId(),
		Name:               req.GetName(),
		Endpoint:           req.GetEndpoint(),
		EndpointType:       int(req.GetEndpointType()),
		APIVersion:         int(req.GetApiVersion()),
		OS:                 req.GetOs(),
		Arch:               req.GetArch(),
		Platform:           req.GetPlatform(),
		UptimeSeconds:      req.GetUptimeSeconds(),
		HeartbeatIntervalS: int(req.GetHeartbeatIntervalS()),
		Metadata:           req.GetMetadata(),
		Capabilities:       wire.CapabilitiesFromProto(req.GetCapabilities()),
	})
}

// recordSyncEvents emits the connection / issue-resolution events shared by both
// Sync transports after a successful upsert. existing and getErr describe the
// agent state observed *before* the upsert (getErr == ErrNotFound ⇒ brand new;
// a non-active existing agent ⇒ reconnect that clears outstanding issues).
func recordSyncEvents(hub *event.Hub, id, name string, existing *model.Agent, getErr error) {
	switch {
	case errors.Is(getErr, storage.ErrNotFound):
		hub.Record(event.AgentConnected, event.Payload{ID: id, Name: name, By: "discovery"})
	case existing != nil && existing.Status() != enum.AgentStatusActive:
		hub.Record(event.AgentConnected, event.Payload{ID: id, Name: name, By: "discovery"})

		n := hub.DeleteIssues(event.AgentInactive, id)
		n += hub.DeleteIssues(event.AgentDisconnected, id)
		if n > 0 {
			hub.Record(event.IssueClosed, event.Payload{ID: id, Name: name, By: "discovery"})
			hub.Notify(event.RefreshDashboard)
		}
	}
	hub.Notify(event.RefreshAgents)
}

// bearerToken extracts the credential from an Authorization header value,
// accepting the scheme case-insensitively. Returns "" if absent/malformed.
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) >= len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return header[len(prefix):]
	}
	return ""
}

// grpcPeerIP returns the canonical IP of the gRPC peer. gRPC requests carry no
// forwarding headers, so the peer address is the client.
func grpcPeerIP(ctx context.Context) string {
	var addr string
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr = p.Addr.String()
	}
	return clientip.FromAddr(addr)
}

// grpcBearerToken extracts the bearer token from incoming gRPC "authorization"
// metadata. Returns "" if absent/malformed.
func grpcBearerToken(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return ""
	}
	return bearerToken(vals[0])
}
