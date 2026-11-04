package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/cluster"
)

// change is a change of the configuration a stubAdmin was asked for.
type change struct {
	op, id, address string
	prevIndex       uint64
}

// stubAdmin is the cluster administration of a node that the test plays: it
// leads a configuration it holds, answers with the errors the test sets, and
// records what it was asked to change. A transfer that succeeds leaves it a
// follower that knows next as its leader, once it was asked unknownFor times.
type stubAdmin struct {
	mu sync.Mutex

	nodeID     string
	role       string
	leader     cluster.Server // the leader it knows; its ID and address only
	next       cluster.Server // the leader a successful transfer leaves it knowing
	unknownFor int            // the asks after a transfer that know no leader yet
	asks       int            // the asks for the leader
	conf       cluster.Configuration

	probeErr, addErr, removeErr, transferErr error

	probes    []string
	changes   []change
	transfers int
}

// leading is a stubAdmin for n1, which leads voters of the IDs, each at
// 10.0.0.<i+1>:7000, in a configuration at index 7.
func leading(ids ...string) *stubAdmin {
	s := &stubAdmin{nodeID: "n1", role: cluster.RoleLeader, conf: cluster.Configuration{Index: 7}}
	for i, id := range ids {
		s.conf.Servers = append(s.conf.Servers, cluster.Server{ID: id, Address: fmt.Sprintf("10.0.0.%d:7000", i+1), Suffrage: cluster.Voter})
	}
	s.leader = s.conf.Servers[0]
	s.next = cluster.Server{ID: "n2", Address: "10.0.0.2:7000"}
	return s
}

func (s *stubAdmin) NodeID() string { return s.nodeID }

func (s *stubAdmin) Role() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.role
}

func (s *stubAdmin) Leader() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asks++
	if s.transfers > 0 && s.unknownFor > 0 {
		s.unknownFor--
		return "", ""
	}
	return s.leader.ID, s.leader.Address
}

func (s *stubAdmin) LastIndex() uint64 { return 42 }

func (s *stubAdmin) Servers() []cluster.Server { return slices.Clone(s.conf.Servers) }

func (s *stubAdmin) Configuration() (cluster.Configuration, error) {
	return cluster.Configuration{Index: s.conf.Index, Servers: slices.Clone(s.conf.Servers)}, nil
}

func (s *stubAdmin) Probe(_ context.Context, address string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probes = append(s.probes, address)
	return s.probeErr
}

func (s *stubAdmin) AddVoter(id, address string, prevIndex uint64) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changes = append(s.changes, change{"add", id, address, prevIndex})
	return prevIndex + 1, s.addErr
}

func (s *stubAdmin) RemoveServer(id string, prevIndex uint64) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changes = append(s.changes, change{"remove", id, "", prevIndex})
	return prevIndex + 1, s.removeErr
}

func (s *stubAdmin) TransferLeadership() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transfers++
	if s.transferErr != nil {
		return s.transferErr
	}
	s.role, s.leader = cluster.RoleFollower, s.next
	return nil
}

// admin is the actor of the changes, with credentials of its session that no
// log line may name.
var admin = &identity.Identity{UserID: "user-admin", Subject: "admin", TokenID: "token-id-never-logged", SessionID: "session-id-never-logged"}

// The rules of an addition refuse, before anything changes: a member without an
// ID or a host:port address, an ID that discovery.peers or the member's route
// cannot name, an ID or an address that a member has already, and an address
// where no node of the cluster answers. A node that does not lead refuses too,
// as not the leader. An addition that passes them is applied against the index
// of the configuration the rules read.
func TestAddMemberRules(t *testing.T) {
	unreachable := fmt.Errorf("raft: probe 10.0.0.4:7000: %w", cluster.ErrPeerUnreachable)
	for _, tc := range []struct {
		name     string
		req      AddRequest
		setup    func(*stubAdmin)
		want     error // nil: added
		probed   bool  // the address was probed
		proposed bool  // AddVoter was called
	}{
		{"a new voter", AddRequest{ID: "n4", Address: "10.0.0.4:7000"}, nil, nil, true, true},
		{"a host name", AddRequest{ID: "n4", Address: "podium-4.podium:7000"}, nil, nil, true, true},
		{"an IPv6 address", AddRequest{ID: "n4", Address: "[fd00::4]:7000"}, nil, nil, true, true},
		{"no ID", AddRequest{Address: "10.0.0.4:7000"}, nil, ErrInvalidMember, false, false},
		{"no address", AddRequest{ID: "n4"}, nil, ErrInvalidMember, false, false},
		{"an address without a port", AddRequest{ID: "n4", Address: "10.0.0.4"}, nil, ErrInvalidMember, false, false},
		{"port 0", AddRequest{ID: "n4", Address: "10.0.0.4:0"}, nil, ErrInvalidMember, false, false},
		{"a port out of range", AddRequest{ID: "n4", Address: "10.0.0.4:65536"}, nil, ErrInvalidMember, false, false},
		{"no host", AddRequest{ID: "n4", Address: ":7000"}, nil, ErrInvalidMember, false, false},
		{"every interface", AddRequest{ID: "n4", Address: "0.0.0.0:7000"}, nil, ErrInvalidMember, false, false},
		{"an ID with a /", AddRequest{ID: "n/4", Address: "10.0.0.4:7000"}, nil, ErrInvalidMember, false, false},
		{"an ID with a =", AddRequest{ID: "n=4", Address: "10.0.0.4:7000"}, nil, ErrInvalidMember, false, false},
		{"an ID with a comma", AddRequest{ID: "n,4", Address: "10.0.0.4:7000"}, nil, ErrInvalidMember, false, false},
		{"an ID with surrounding spaces", AddRequest{ID: " n4", Address: "10.0.0.4:7000"}, nil, ErrInvalidMember, false, false},
		{"the ID of a member", AddRequest{ID: "n2", Address: "10.0.0.4:7000"}, nil, ErrMemberExists, false, false},
		{"the address of a member", AddRequest{ID: "n4", Address: "10.0.0.2:7000"}, nil, ErrAddressInUse, false, false},
		{"no node answers", AddRequest{ID: "n4", Address: "10.0.0.4:7000"}, func(s *stubAdmin) { s.probeErr = unreachable }, cluster.ErrPeerUnreachable, true, false},
		{"not the leader", AddRequest{ID: "n4", Address: "10.0.0.4:7000"}, func(s *stubAdmin) { s.role = cluster.RoleFollower }, cluster.ErrNotLeader, false, false},
		{"a configuration changed meanwhile", AddRequest{ID: "n4", Address: "10.0.0.4:7000"}, func(s *stubAdmin) {
			s.addErr = fmt.Errorf("raft: %w", cluster.ErrConfigurationChanged)
		}, cluster.ErrConfigurationChanged, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raft := leading("n1", "n2", "n3")
			if tc.setup != nil {
				tc.setup(raft)
			}
			err := New(raft, zerolog.Nop()).AddMember(context.Background(), admin, tc.req)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("AddMember(%+v) = %v, want nil", tc.req, err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("AddMember(%+v) = %v, want %v", tc.req, err, tc.want)
			}
			if probed := len(raft.probes) > 0; probed != tc.probed || (probed && raft.probes[0] != tc.req.Address) {
				t.Fatalf("probes %v, want a probe of %s: %v", raft.probes, tc.req.Address, tc.probed)
			}
			want := []change(nil)
			if tc.proposed {
				want = []change{{"add", tc.req.ID, tc.req.Address, 7}}
			}
			if !slices.Equal(raft.changes, want) {
				t.Fatalf("changes %v, want %v: against the index the rules read", raft.changes, want)
			}
		})
	}
}

// A removal that leaves fewer than three voters needs force, and the last voter
// cannot be removed, force or not: hashicorp/raft has no configuration without
// a voter. The guard counts the configuration the removal is applied against,
// whose index it passes on. An ID no member has is unknown.
func TestRemoveMemberForceGuard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		voters []string
		id     string
		force  bool
		want   error // nil: removed
	}{
		{"four voters leave three", []string{"n1", "n2", "n3", "n4"}, "n4", false, nil},
		{"three voters leave two", []string{"n1", "n2", "n3"}, "n3", false, ErrForceRequired},
		{"three voters leave two, with force", []string{"n1", "n2", "n3"}, "n3", true, nil},
		{"two voters leave one", []string{"n1", "n2"}, "n2", false, ErrForceRequired},
		{"two voters leave one, with force", []string{"n1", "n2"}, "n2", true, nil},
		{"the last voter", []string{"n1"}, "n1", false, ErrLastVoter},
		{"the last voter, with force", []string{"n1"}, "n1", true, ErrLastVoter},
		{"an unknown ID", []string{"n1", "n2", "n3"}, "n9", true, ErrUnknownMember},
		{"no ID", []string{"n1", "n2", "n3"}, "", true, ErrInvalidMember},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raft := leading(tc.voters...)
			err := New(raft, zerolog.Nop()).RemoveMember(context.Background(), admin, RemoveRequest{ID: tc.id, Force: tc.force})
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("RemoveMember(%s, force %v) = %v, want nil", tc.id, tc.force, err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("RemoveMember(%s, force %v) = %v, want %v", tc.id, tc.force, err, tc.want)
			}
			want := []change(nil)
			if tc.want == nil {
				want = []change{{"remove", tc.id, "", 7}}
			}
			if !slices.Equal(raft.changes, want) || raft.transfers != 0 {
				t.Fatalf("changes %v and %d transfers, want %v and none", raft.changes, raft.transfers, want)
			}
		})
	}
}

// The leader that is asked to remove itself first hands its leadership over and
// removes nothing: it answers that leadership moved, a not-the-leader error,
// once it knows the new leader, so that the answer can name it, and the removal
// is repeated there. A transfer that fails removes nothing either, and the node
// still leads. The guard still comes first: a removal it refuses transfers
// nothing.
func TestRemovingTheLeaderHandsItsLeadershipOverFirst(t *testing.T) {
	ctx := context.Background()
	t.Run("transferred", func(t *testing.T) {
		raft := leading("n1", "n2", "n3")
		err := New(raft, zerolog.Nop()).RemoveMember(ctx, admin, RemoveRequest{ID: "n1", Force: true})
		if !errors.Is(err, ErrLeadershipMoved) || !errors.Is(err, cluster.ErrNotLeader) {
			t.Fatalf("RemoveMember(n1) = %v, want %v, a not-the-leader error", err, ErrLeadershipMoved)
		}
		if raft.transfers != 1 || len(raft.changes) != 0 {
			t.Fatalf("%d transfers and the changes %v, want one transfer and no change", raft.transfers, raft.changes)
		}
		if id, _ := raft.Leader(); id != "n2" {
			t.Fatalf("the node knows %q as its leader, want n2", id)
		}
	})
	t.Run("the new leader known later", func(t *testing.T) {
		raft := leading("n1", "n2", "n3")
		raft.unknownFor = 3
		if err := New(raft, zerolog.Nop()).RemoveMember(ctx, admin, RemoveRequest{ID: "n1", Force: true}); !errors.Is(err, ErrLeadershipMoved) {
			t.Fatalf("RemoveMember(n1) = %v, want %v", err, ErrLeadershipMoved)
		}
		raft.mu.Lock()
		defer raft.mu.Unlock()
		if raft.unknownFor != 0 {
			t.Fatalf("the removal answered while the node still knew no leader: %d asks to go", raft.unknownFor)
		}
	})
	t.Run("no new leader within the bound", func(t *testing.T) {
		raft := leading("n1", "n2", "n3")
		raft.next = cluster.Server{}
		svc := New(raft, zerolog.Nop())
		svc.leaderWait = 20 * time.Millisecond
		if err := svc.RemoveMember(ctx, admin, RemoveRequest{ID: "n1", Force: true}); !errors.Is(err, ErrLeadershipMoved) {
			t.Fatalf("RemoveMember(n1) = %v, want %v once the bound passed", err, ErrLeadershipMoved)
		}
	})
	t.Run("a failed transfer", func(t *testing.T) {
		raft := leading("n1", "n2", "n3")
		raft.transferErr = errors.New("leadership transfer timeout")
		err := New(raft, zerolog.Nop()).RemoveMember(ctx, admin, RemoveRequest{ID: "n1", Force: true})
		if !errors.Is(err, ErrTransferFailed) || errors.Is(err, cluster.ErrNotLeader) {
			t.Fatalf("RemoveMember(n1) = %v, want %v and no not-the-leader error", err, ErrTransferFailed)
		}
		if len(raft.changes) != 0 || raft.Role() != cluster.RoleLeader {
			t.Fatalf("the changes %v and the role %s, want none and leader", raft.changes, raft.Role())
		}
	})
	t.Run("refused by the guard", func(t *testing.T) {
		raft := leading("n1", "n2", "n3")
		if err := New(raft, zerolog.Nop()).RemoveMember(ctx, admin, RemoveRequest{ID: "n1"}); !errors.Is(err, ErrForceRequired) {
			t.Fatalf("RemoveMember(n1) = %v, want %v", err, ErrForceRequired)
		}
		if raft.transfers != 0 {
			t.Fatal("a removal the guard refused transferred the leadership")
		}
	})
}

// Every membership change logs one INFO line: who changed what, the actor's user
// ID, the operation, the member's ID and address, and force. The credentials of
// the actor's session never reach the log. A refused change logs no such line.
func TestMembershipChangesLogOneInfoLine(t *testing.T) {
	var logs bytes.Buffer
	raft := leading("n1", "n2", "n3", "n4")
	svc := New(raft, zerolog.New(&logs))
	ctx := context.Background()
	if err := svc.AddMember(ctx, admin, AddRequest{ID: "n5", Address: "10.0.0.5:7000"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveMember(ctx, admin, RemoveRequest{ID: "n4", Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddMember(ctx, admin, AddRequest{ID: "n2", Address: "10.0.0.9:7000"}); !errors.Is(err, ErrMemberExists) {
		t.Fatalf("a refused addition: %v", err)
	}

	out := logs.String()
	for _, secret := range []string{admin.TokenID, admin.SessionID} {
		if strings.Contains(out, secret) {
			t.Fatalf("the log names %q: %s", secret, out)
		}
	}
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if fields["level"] == "info" {
			lines = append(lines, fields)
		}
	}
	want := []map[string]any{
		{"level": "info", "service": "cluster", "actor_user_id": "user-admin", "operation": "add", "member_id": "n5", "member_address": "10.0.0.5:7000", "force": false, "message": "cluster membership changed"},
		{"level": "info", "service": "cluster", "actor_user_id": "user-admin", "operation": "remove", "member_id": "n4", "member_address": "10.0.0.4:7000", "force": true, "message": "cluster membership changed"},
	}
	if len(lines) != len(want) {
		t.Fatalf("%d INFO lines, want %d: %s", len(lines), len(want), out)
	}
	for i := range want {
		if fmt.Sprint(lines[i]) != fmt.Sprint(want[i]) {
			t.Fatalf("INFO line %d = %v, want %v", i, lines[i], want[i])
		}
	}
}

// The status is the node's own view: its ID and role, the leader it knows, the
// servers of its latest configuration and the last index of its log.
func TestStatusIsTheNodesView(t *testing.T) {
	raft := leading("n1", "n2", "n3")
	raft.role, raft.leader = cluster.RoleFollower, cluster.Server{ID: "n2", Address: "10.0.0.2:7000"}
	got := New(raft, zerolog.Nop()).Status(context.Background())
	want := Status{NodeID: "n1", Role: cluster.RoleFollower, LeaderID: "n2", LeaderAddr: "10.0.0.2:7000", Members: raft.conf.Servers, LastIndex: 42}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Status() = %+v, want %+v", got, want)
	}
}
