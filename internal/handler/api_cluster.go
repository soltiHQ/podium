package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/cluster"
	clustersvc "github.com/soltiHQ/control-plane/internal/service/cluster"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
)

// Cluster handles /api/v1/cluster, which a raft node alone serves.
//
// Supported:
//   - GET /api/v1/cluster
func (a *API) Cluster(w http.ResponseWriter, r *http.Request) {
	route.Resource(w, r, routepath.ApiCluster,
		route.Endpoint{Method: http.MethodGet, Perm: enum.ClusterGet, Fn: a.clusterStatus},
	)
}

// ClusterMembers handles /api/v1/cluster/members, which a raft node alone serves.
//
// Supported:
//   - POST /api/v1/cluster/members
func (a *API) ClusterMembers(w http.ResponseWriter, r *http.Request) {
	route.Resource(w, r, routepath.ApiClusterMembers,
		route.Endpoint{Method: http.MethodPost, Perm: enum.ClusterManage, Fn: a.clusterMemberAdd},
	)
}

// ClusterMembersRouter handles /api/v1/cluster/members/{id}, which a raft node
// alone serves.
//
// Supported:
//   - DELETE /api/v1/cluster/members/{id}
func (a *API) ClusterMembersRouter(w http.ResponseWriter, r *http.Request) {
	route.Router(w, r, routepath.ApiClusterMember,
		route.Subroute{Action: "", Method: http.MethodDelete, Perm: enum.ClusterManage, Fn: a.clusterMemberRemove},
	)
}

func (a *API) clusterStatus(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	st := a.clusterSVC.Status(r.Context())
	members := make([]restv1.ClusterMember, 0, len(st.Members))
	for _, m := range st.Members {
		members = append(members, restv1.ClusterMember{ID: m.ID, Address: m.Address, Suffrage: string(m.Suffrage)})
	}
	response.OK(w, r, mode, &responder.View{
		Data: restv1.Cluster{
			NodeID:        st.NodeID,
			Role:          st.Role,
			LeaderID:      st.LeaderID,
			LeaderAddress: st.LeaderAddr,
			Members:       members,
			LastIndex:     st.LastIndex,
		},
	})
}

func (a *API) clusterMemberAdd(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	in, err := decodeJSON[restv1.AddClusterMemberRequest](r)
	if err != nil {
		badBody(w, r, mode, err)
		return
	}
	err = a.clusterSVC.AddMember(r.Context(), a.identity(r), clustersvc.AddRequest{ID: in.ID, Address: in.Address})
	if err != nil {
		a.clusterChangeFailed(w, r, mode, err, in.ID, in.Address)
		return
	}
	response.NoContent(w, r)
}

func (a *API) clusterMemberRemove(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	force := false
	if raw := r.URL.Query().Get("force"); raw != "" {
		var err error
		if force, err = strconv.ParseBool(raw); err != nil {
			response.BadRequestMsg(w, r, mode, "force must be true or false.")
			return
		}
	}
	err := a.clusterSVC.RemoveMember(r.Context(), a.identity(r), clustersvc.RemoveRequest{ID: id, Force: force})
	if err != nil {
		a.clusterChangeFailed(w, r, mode, err, id, "")
		return
	}
	response.NoContent(w, r)
}

// clusterChangeFailed answers a membership change that the cluster service
// refused or that did not take place. Each changed nothing, but a change that
// lost leadership while it committed (cluster.ErrLeadershipLost), which may
// still take effect: like every leadership error it answers 503 with X-Leader,
// and the service logged it, with its actor, as of unknown outcome. id is the
// member's, and address the address of a member to add, which a failed probe
// names.
func (a *API) clusterChangeFailed(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, err error, id, address string) {
	var inUse *clustersvc.AddressInUseError
	switch {
	case errors.Is(err, clustersvc.ErrInvalidMember):
		response.BadRequestMsg(w, r, mode, "A member needs an id without /, =, a comma or surrounding spaces, and an address host:port with a port from 1 to 65535.")
	case errors.Is(err, clustersvc.ErrUnknownMember):
		response.NotFound(w, r, mode)
	case errors.Is(err, clustersvc.ErrMemberExists):
		response.Conflict(w, r, mode, "A member of the cluster has this id already.")
	case errors.As(err, &inUse):
		response.Conflict(w, r, mode, fmt.Sprintf("Member %s of the cluster answers at %s already.", inUse.Member, inUse.At))
	case errors.Is(err, clustersvc.ErrForceRequired):
		response.Conflict(w, r, mode, "The removal would leave fewer than 3 voters: repeat it with force=true to remove the member anyway.")
	case errors.Is(err, clustersvc.ErrLastVoter):
		response.Conflict(w, r, mode, "The last voter cannot be removed.")
	case errors.Is(err, cluster.ErrConfigurationChanged):
		response.Conflict(w, r, mode, "configuration changed, retry")
	case errors.Is(err, cluster.ErrPeerUnreachable):
		response.UnprocessableEntity(w, r, mode, fmt.Sprintf("No raft node of this cluster answers at %s: check the address, that the node runs, and its certificate. Nothing was changed.", address))
	case errors.Is(err, clustersvc.ErrLeadershipMoved) && httpctx.Leader(r.Context()) == "":
		// No new leader known within the wait, or none derivable: no X-Leader to name.
		response.UnavailableMsg(w, r, mode, "Leadership moved; no leader is known yet, retry.")
	case errors.Is(err, clustersvc.ErrLeadershipMoved):
		response.NotLeader(w, r, mode, "This node handed its leadership over and is still a member: repeat the request, at the leader that X-Leader names, to remove it.")
	case errors.Is(err, clustersvc.ErrTransferFailed):
		a.logger.Warn().Err(err).Str("member_id", id).Msg("cluster member removal failed: the leadership transfer failed, nothing was removed")
		response.UnavailableMsg(w, r, mode, "The leadership transfer failed, so nothing was removed: remove any dead member first, then retry.")
	case errors.Is(err, clustersvc.ErrConfigurationUnreadable):
		// The service logged it at ERROR.
		response.FromError(w, r, mode, err)
	case errors.Is(err, cluster.ErrLeadershipLost):
		// The service logged it, with its actor, as of unknown outcome.
		response.StoreFailed(w, r, mode, err)
	default:
		a.failure(err).Str("member_id", id).Msg("cluster membership change failed")
		response.StoreFailed(w, r, mode, err)
	}
}
