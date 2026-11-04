package cluster

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/cluster"
)

// minVoters is the least number of voters a removal leaves without force: three
// voters still have a quorum when one fails.
const minVoters = 3

// defaultLeaderWait bounds how long a removal of the leader waits, once it
// handed its leadership over, for this node to know the new leader, which its
// answer names.
const defaultLeaderWait = 5 * time.Second

// leaderPoll is how often that wait asks.
const leaderPoll = 10 * time.Millisecond

// Service reads and changes the membership of the raft cluster of the node
// whose administration it is given. Every change runs on the leader, is checked
// against the latest configuration read with its index, and is applied against
// that index: a change of the configuration in between refuses it with
// cluster.ErrConfigurationChanged instead of letting it past the rules.
type Service struct {
	raft   cluster.Admin
	logger zerolog.Logger
	// leaderWait is defaultLeaderWait; tests shorten it.
	leaderWait time.Duration
}

// New creates a cluster service over admin.
func New(admin cluster.Admin, logger zerolog.Logger) *Service {
	if admin == nil {
		panic("cluster.Service: admin is nil")
	}
	return &Service{
		raft:       admin,
		logger:     logger.With().Str("service", "cluster").Logger(),
		leaderWait: defaultLeaderWait,
	}
}

// Status returns the cluster as this node sees it, from the copy of its
// configuration that raft keeps: it waits for nothing.
func (s *Service) Status(_ context.Context) Status {
	leaderID, leaderAddr := s.raft.Leader()
	return Status{
		NodeID:     s.raft.NodeID(),
		Role:       s.raft.Role(),
		LeaderID:   leaderID,
		LeaderAddr: leaderAddr,
		Members:    s.raft.Servers(),
		LastIndex:  s.raft.LastIndex(),
	}
}

// AddMember adds a voter, on the leader, and refuses with nothing changed:
//   - a member with a malformed ID or address (ErrInvalidMember);
//   - an ID, or an address as written, that a member has (ErrMemberExists,
//     ErrAddressInUse);
//   - an address where no node of the cluster answers as the raft transport
//     would dial it (cluster.ErrPeerUnreachable): a typo must not add a voter
//     nobody reaches, which would raise the quorum.
//
// A node that does not lead refuses with cluster.ErrNotLeader.
func (s *Service) AddMember(ctx context.Context, actor *identity.Identity, req AddRequest) error {
	if err := checkMember(req.ID, req.Address); err != nil {
		return err
	}
	conf, err := s.leaderConfiguration()
	if err != nil {
		return err
	}
	for _, m := range conf.Servers {
		switch {
		case m.ID == req.ID:
			return ErrMemberExists
		case m.Address == req.Address:
			return ErrAddressInUse
		}
	}
	if err := s.raft.Probe(ctx, req.Address); err != nil {
		s.logger.Warn().Err(err).Str("member_id", req.ID).Str("member_address", req.Address).
			Msg("cluster member not added: no node of the cluster answers at its address")
		return err
	}
	if _, err := s.raft.AddVoter(req.ID, req.Address, conf.Index); err != nil {
		return err
	}
	s.changed(actor, "add", req.ID, req.Address, false)
	return nil
}

// RemoveMember removes a member, on the leader, under the force guard: a
// removal that leaves fewer than minVoters voters needs force
// (ErrForceRequired), and the last voter cannot be removed at all
// (ErrLastVoter). An ID no member has is ErrUnknownMember.
//
// The leader asked to remove itself first hands its leadership over to another
// voter, so that writes resume without an election timeout, and removes nothing:
// it answers ErrLeadershipMoved once it knows the new leader, or once
// leaderWait passed, and the removal is repeated at the new leader, where it
// removes a follower. A transfer that fails removes nothing either
// (ErrTransferFailed). A node that does not lead refuses with
// cluster.ErrNotLeader.
func (s *Service) RemoveMember(ctx context.Context, actor *identity.Identity, req RemoveRequest) error {
	if req.ID == "" {
		return ErrInvalidMember
	}
	conf, err := s.leaderConfiguration()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(conf.Servers, func(m cluster.Server) bool { return m.ID == req.ID })
	if i < 0 {
		return ErrUnknownMember
	}
	member := conf.Servers[i]
	if member.Suffrage == cluster.Voter {
		switch left := conf.Voters() - 1; {
		case left == 0:
			return ErrLastVoter
		case left < minVoters && !req.Force:
			return ErrForceRequired
		}
	}
	if member.ID == s.raft.NodeID() {
		return s.handOver(ctx, actor, member, req.Force)
	}
	if _, err := s.raft.RemoveServer(member.ID, conf.Index); err != nil {
		return err
	}
	s.changed(actor, "remove", member.ID, member.Address, req.Force)
	return nil
}

// leaderConfiguration returns the latest configuration with its index, on the
// leader, and cluster.ErrNotLeader on any other node.
func (s *Service) leaderConfiguration() (cluster.Configuration, error) {
	if role := s.raft.Role(); role != cluster.RoleLeader {
		return cluster.Configuration{}, fmt.Errorf("cluster: a membership change on a %s: %w", role, cluster.ErrNotLeader)
	}
	return s.raft.Configuration()
}

// handOver transfers this node's leadership before its removal, and waits until
// it knows the new leader (awaitNewLeader).
func (s *Service) handOver(ctx context.Context, actor *identity.Identity, member cluster.Server, force bool) error {
	if err := s.raft.TransferLeadership(); err != nil {
		if cluster.IsLeadershipError(err) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrTransferFailed, err)
	}
	s.awaitNewLeader(ctx, member.ID)
	leaderID, leaderAddr := s.raft.Leader()
	s.logger.Info().Str("actor_user_id", actorID(actor)).Str("operation", "transfer").
		Str("member_id", member.ID).Str("member_address", member.Address).Bool("force", force).
		Str("leader_id", leaderID).Str("leader_address", leaderAddr).
		Msg("cluster leadership handed over before the removal of this node: the removal is repeated at the new leader")
	return ErrLeadershipMoved
}

// awaitNewLeader waits until this node knows a leader other than self, up to
// leaderWait or the end of ctx: a node learns its new leader from the first
// heartbeat the new leader sends.
func (s *Service) awaitNewLeader(ctx context.Context, self string) {
	wait := time.NewTimer(s.leaderWait)
	defer wait.Stop()
	poll := time.NewTicker(leaderPoll)
	defer poll.Stop()
	for {
		if id, _ := s.raft.Leader(); id != "" && id != self {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-wait.C:
			return
		case <-poll.C:
		}
	}
}

// changed logs a membership change: one INFO line with the actor's user ID, the
// operation, the member and force, and nothing of the actor's credentials.
func (s *Service) changed(actor *identity.Identity, op, id, address string, force bool) {
	s.logger.Info().Str("actor_user_id", actorID(actor)).Str("operation", op).
		Str("member_id", id).Str("member_address", address).Bool("force", force).
		Msg("cluster membership changed")
}

// actorID is the user ID of actor, "unknown" without one.
func actorID(actor *identity.Identity) string {
	if actor == nil || actor.UserID == "" {
		return "unknown"
	}
	return actor.UserID
}

// checkMember refuses, as ErrInvalidMember, a member without an ID or an
// address, an ID with a / that its route could not carry, a = or a comma that
// discovery.peers could not, or spaces around it, which discovery.peers trims,
// and an address that peers could not dial (cluster.CheckDialAddress).
func checkMember(id, address string) error {
	switch {
	case id == "" || address == "",
		strings.ContainsAny(id, "/=,"),
		strings.TrimSpace(id) != id:
		return ErrInvalidMember
	}
	if err := cluster.CheckDialAddress(address); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidMember, err)
	}
	return nil
}
