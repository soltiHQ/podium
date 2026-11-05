package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
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

	confErr, probeErr, addErr, removeErr, transferErr, verifyErr error

	probes    []string
	changes   []change
	transfers int
	verified  []uint64 // the term of each check of the leadership
}

// leading is a stubAdmin for n1, which leads voters of the IDs, each at
// 10.0.0.<i+1>:7000, in a configuration at index 7 that it read in term 4.
func leading(ids ...string) *stubAdmin {
	s := &stubAdmin{nodeID: "n1", role: cluster.RoleLeader, conf: cluster.Configuration{Term: 4, Index: 7}}
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
	return cluster.Configuration{Term: s.conf.Term, Index: s.conf.Index, Servers: slices.Clone(s.conf.Servers)}, s.confErr
}

func (s *stubAdmin) VerifyLeader(_ context.Context, term uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verified = append(s.verified, term)
	return s.verifyErr
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

// names is the DNS of these tests: a resolver that knows its names, each at its
// IPs, and no other name.
type names map[string][]string

func (n names) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := n[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out, nil
}

// newService is New over raft, which looks hosts up in the names of the tests:
// podium-2.podium is at 10.0.0.2, where leading puts n2, and podium-4.podium at
// 10.0.0.4.
func newService(raft *stubAdmin, logger zerolog.Logger) *Service {
	svc := New(raft, logger)
	svc.resolver = names{"podium-2.podium": {"10.0.0.2"}, "podium-4.podium": {"10.0.0.4"}}
	return svc
}

// admin is the actor of the changes, with credentials of its session that no
// log line may name.
var admin = &identity.Identity{UserID: "user-admin", Subject: "admin", TokenID: "token-id-never-logged", SessionID: "session-id-never-logged"}

// The rules of an addition refuse, before anything changes: a member without an
// ID or a host:port address, an ID that discovery.peers or the member's route
// cannot name, an ID that a member has already, an address at which a member
// answers already, as written or under another name of its ip:port, which the
// refusal names, and an address where no node of the cluster answers. A
// member's name that does not resolve, as a dead member's can, counts for
// nothing, and a new name that does not resolve is left to the probe. A node
// that does not lead refuses too, as not the leader. An addition that passes
// them is applied against the index of the configuration the rules read.
func TestAddMemberRules(t *testing.T) {
	unreachable := fmt.Errorf("raft: probe 10.0.0.4:7000: %w", cluster.ErrPeerUnreachable)
	for _, tc := range []struct {
		name     string
		req      AddRequest
		setup    func(*stubAdmin)
		want     error  // nil: added
		holder   string // the member an AddressInUseError names
		probed   bool   // the address was probed
		proposed bool   // AddVoter was called
	}{
		{"a new voter", AddRequest{ID: "n4", Address: "10.0.0.4:7000"}, nil, nil, "", true, true},
		{"a host name", AddRequest{ID: "n4", Address: "podium-4.podium:7000"}, nil, nil, "", true, true},
		{"an IPv6 address", AddRequest{ID: "n4", Address: "[fd00::4]:7000"}, nil, nil, "", true, true},
		{"no ID", AddRequest{Address: "10.0.0.4:7000"}, nil, ErrInvalidMember, "", false, false},
		{"no address", AddRequest{ID: "n4"}, nil, ErrInvalidMember, "", false, false},
		{"an address without a port", AddRequest{ID: "n4", Address: "10.0.0.4"}, nil, ErrInvalidMember, "", false, false},
		{"port 0", AddRequest{ID: "n4", Address: "10.0.0.4:0"}, nil, ErrInvalidMember, "", false, false},
		{"a port out of range", AddRequest{ID: "n4", Address: "10.0.0.4:65536"}, nil, ErrInvalidMember, "", false, false},
		{"no host", AddRequest{ID: "n4", Address: ":7000"}, nil, ErrInvalidMember, "", false, false},
		{"every interface", AddRequest{ID: "n4", Address: "0.0.0.0:7000"}, nil, ErrInvalidMember, "", false, false},
		{"an ID with a /", AddRequest{ID: "n/4", Address: "10.0.0.4:7000"}, nil, ErrInvalidMember, "", false, false},
		{"an ID with a =", AddRequest{ID: "n=4", Address: "10.0.0.4:7000"}, nil, ErrInvalidMember, "", false, false},
		{"an ID with a comma", AddRequest{ID: "n,4", Address: "10.0.0.4:7000"}, nil, ErrInvalidMember, "", false, false},
		{"an ID with surrounding spaces", AddRequest{ID: " n4", Address: "10.0.0.4:7000"}, nil, ErrInvalidMember, "", false, false},
		{"the ID of a member", AddRequest{ID: "n2", Address: "10.0.0.4:7000"}, nil, ErrMemberExists, "", false, false},
		{"the address of a member", AddRequest{ID: "n4", Address: "10.0.0.2:7000"}, nil, ErrAddressInUse, "n2", false, false},
		{"another name of a member", AddRequest{ID: "n4", Address: "podium-2.podium:7000"}, nil, ErrAddressInUse, "n2", false, false},
		{"the IP of a member listed by name", AddRequest{ID: "n4", Address: "10.0.0.2:7000"}, func(s *stubAdmin) {
			s.conf.Servers[1].Address = "podium-2.podium:7000"
		}, ErrAddressInUse, "n2", false, false},
		{"another name of a member's host, at another port", AddRequest{ID: "n4", Address: "podium-2.podium:7009"}, nil, nil, "", true, true},
		{"a dead member listed by a name that does not resolve", AddRequest{ID: "n4", Address: "10.0.0.4:7000"}, func(s *stubAdmin) {
			s.conf.Servers[2].Address = "podium-3.podium:7000"
		}, nil, "", true, true},
		{"a new name that does not resolve, left to the probe", AddRequest{ID: "n4", Address: "podium-9.podium:7000"}, nil, nil, "", true, true},
		{"no node answers", AddRequest{ID: "n4", Address: "10.0.0.4:7000"}, func(s *stubAdmin) { s.probeErr = unreachable }, cluster.ErrPeerUnreachable, "", true, false},
		{"not the leader", AddRequest{ID: "n4", Address: "10.0.0.4:7000"}, func(s *stubAdmin) { s.role = cluster.RoleFollower }, cluster.ErrNotLeader, "", false, false},
		{"a configuration changed meanwhile", AddRequest{ID: "n4", Address: "10.0.0.4:7000"}, func(s *stubAdmin) {
			s.addErr = fmt.Errorf("raft: %w", cluster.ErrConfigurationChanged)
		}, cluster.ErrConfigurationChanged, "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raft := leading("n1", "n2", "n3")
			if tc.setup != nil {
				tc.setup(raft)
			}
			err := newService(raft, zerolog.Nop()).AddMember(context.Background(), admin, tc.req)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("AddMember(%+v) = %v, want nil", tc.req, err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("AddMember(%+v) = %v, want %v", tc.req, err, tc.want)
			}
			var inUse *AddressInUseError
			if tc.holder != "" && (!errors.As(err, &inUse) || inUse.Member != tc.holder) {
				t.Fatalf("AddMember(%+v) = %v, want the member %s named", tc.req, err, tc.holder)
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

// A refusal that the rules read from the node's own configuration, an ID or an
// address in use, an unknown ID, the force guard or the last voter, is answered
// only once a quorum confirmed that the node still leads the term it read the
// configuration in: a deposed leader reads a stale configuration, and would
// refuse on it. A check that fails answers cluster.ErrNotLeader in place of the
// refusal, and changes nothing. A refusal of the request itself checks
// nothing, and neither does a change that the rules let through, which a
// deposed leader cannot commit.
func TestRefusalsFromTheConfigurationConfirmTheLeadership(t *testing.T) {
	add := func(id, address string) func(*Service, *stubAdmin) error {
		return func(svc *Service, _ *stubAdmin) error {
			return svc.AddMember(context.Background(), admin, AddRequest{ID: id, Address: address})
		}
	}
	remove := func(id string, force bool, voters ...string) func(*Service, *stubAdmin) error {
		return func(svc *Service, raft *stubAdmin) error {
			if len(voters) > 0 {
				raft.conf.Servers = leading(voters...).conf.Servers
			}
			return svc.RemoveMember(context.Background(), admin, RemoveRequest{ID: id, Force: force})
		}
	}
	for _, tc := range []struct {
		name    string
		change  func(*Service, *stubAdmin) error
		refusal error // nil: no refusal of the configuration's
		checked bool
	}{
		{"an ID in use", add("n2", "10.0.0.4:7000"), ErrMemberExists, true},
		{"an address in use", add("n4", "10.0.0.2:7000"), ErrAddressInUse, true},
		{"another name of a member's address", add("n4", "podium-2.podium:7000"), ErrAddressInUse, true},
		{"an unknown ID", remove("n9", true), ErrUnknownMember, true},
		{"a removal that needs force", remove("n3", false), ErrForceRequired, true},
		{"the last voter", remove("n1", true, "n1"), ErrLastVoter, true},
		{"a member without an ID", add("", "10.0.0.4:7000"), ErrInvalidMember, false},
		{"an addition", add("n4", "10.0.0.4:7000"), nil, false},
		{"a removal", remove("n3", true), nil, false},
	} {
		for _, confirmed := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, leadership confirmed %v", tc.name, confirmed), func(t *testing.T) {
				raft := leading("n1", "n2", "n3")
				if !confirmed {
					raft.verifyErr = fmt.Errorf("raft: confirm the leadership: %w", cluster.ErrNotLeader)
				}
				before := len(raft.changes)
				err := tc.change(newService(raft, zerolog.Nop()), raft)
				want := tc.refusal
				if tc.checked && !confirmed {
					want = cluster.ErrNotLeader
				}
				switch {
				case want == nil && err != nil:
					t.Fatalf("the change = %v, want nil", err)
				case want != nil && !errors.Is(err, want):
					t.Fatalf("the change = %v, want %v", err, want)
				case tc.checked && !confirmed && errors.Is(err, tc.refusal):
					t.Fatalf("the change = %v, the refusal of a configuration whose term was not confirmed", err)
				}
				if tc.checked && len(raft.changes) != before {
					t.Fatalf("a refused change made the changes %v", raft.changes[before:])
				}
				wantChecks := []uint64(nil)
				if tc.checked {
					wantChecks = []uint64{4}
				}
				if !slices.Equal(raft.verified, wantChecks) {
					t.Fatalf("checked the leadership of the terms %v, want %v", raft.verified, wantChecks)
				}
			})
		}
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
			err := newService(raft, zerolog.Nop()).RemoveMember(context.Background(), admin, RemoveRequest{ID: tc.id, Force: tc.force})
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
// is repeated there. A transfer that fails removes nothing either; whether the
// node still leads after it is not known, as a transfer that timed out can still
// see its target win the election it was told to start. The guard still comes
// first: a removal it refuses transfers nothing.
func TestRemovingTheLeaderHandsItsLeadershipOverFirst(t *testing.T) {
	ctx := context.Background()
	t.Run("transferred", func(t *testing.T) {
		raft := leading("n1", "n2", "n3")
		err := newService(raft, zerolog.Nop()).RemoveMember(ctx, admin, RemoveRequest{ID: "n1", Force: true})
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
		if err := newService(raft, zerolog.Nop()).RemoveMember(ctx, admin, RemoveRequest{ID: "n1", Force: true}); !errors.Is(err, ErrLeadershipMoved) {
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
		svc := newService(raft, zerolog.Nop())
		svc.leaderWait = 20 * time.Millisecond
		if err := svc.RemoveMember(ctx, admin, RemoveRequest{ID: "n1", Force: true}); !errors.Is(err, ErrLeadershipMoved) {
			t.Fatalf("RemoveMember(n1) = %v, want %v once the bound passed", err, ErrLeadershipMoved)
		}
	})
	t.Run("a failed transfer", func(t *testing.T) {
		raft := leading("n1", "n2", "n3")
		raft.transferErr = errors.New("leadership transfer timeout")
		err := newService(raft, zerolog.Nop()).RemoveMember(ctx, admin, RemoveRequest{ID: "n1", Force: true})
		if !errors.Is(err, ErrTransferFailed) || errors.Is(err, cluster.ErrNotLeader) {
			t.Fatalf("RemoveMember(n1) = %v, want %v and no not-the-leader error", err, ErrTransferFailed)
		}
		if len(raft.changes) != 0 || raft.Role() != cluster.RoleLeader {
			t.Fatalf("the changes %v and the role %s, want none and leader", raft.changes, raft.Role())
		}
	})
	t.Run("refused by the guard", func(t *testing.T) {
		raft := leading("n1", "n2", "n3")
		if err := newService(raft, zerolog.Nop()).RemoveMember(ctx, admin, RemoveRequest{ID: "n1"}); !errors.Is(err, ErrForceRequired) {
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
	svc := newService(raft, zerolog.New(&logs))
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

// logLines parses what a service logged, one JSON object per line.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		lines = append(lines, fields)
	}
	return lines
}

// A leader whose stores give no configuration to check a change against
// refuses every change before it probes or proposes anything, as
// ErrConfigurationUnreadable, which errkind classifies as Internal, with one
// ERROR line: a read that fails, a configuration at index 0, which as prevIndex
// would turn hashicorp/raft's check off, and one with no server, which every
// rule would pass.
func TestUnreadableConfigurationRefusesEveryChange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*stubAdmin)
	}{
		{"a read that fails", func(s *stubAdmin) { s.confErr = errors.New("raft: read log 9: bolt: database not open") }},
		{"index 0", func(s *stubAdmin) { s.conf.Index = 0 }},
		{"no server", func(s *stubAdmin) { s.conf.Servers = nil }},
	} {
		for _, change := range []struct {
			name string
			run  func(*Service) error
		}{
			{"add", func(svc *Service) error {
				return svc.AddMember(context.Background(), admin, AddRequest{ID: "n4", Address: "10.0.0.4:7000"})
			}},
			{"remove", func(svc *Service) error {
				return svc.RemoveMember(context.Background(), admin, RemoveRequest{ID: "n3", Force: true})
			}},
		} {
			t.Run(tc.name+", "+change.name, func(t *testing.T) {
				raft := leading("n1", "n2", "n3")
				tc.setup(raft)
				var logs bytes.Buffer
				err := change.run(newService(raft, zerolog.New(&logs)))
				if !errors.Is(err, ErrConfigurationUnreadable) {
					t.Fatalf("%v, want %v", err, ErrConfigurationUnreadable)
				}
				if kind := errkind.Classify(err); kind != errkind.Internal {
					t.Fatalf("errkind.Classify(%v) = %v, want Internal", err, kind)
				}
				if len(raft.probes) != 0 || len(raft.changes) != 0 || raft.transfers != 0 {
					t.Fatalf("probes %v, changes %v and %d transfers, want none", raft.probes, raft.changes, raft.transfers)
				}
				lines := logLines(t, &logs)
				if len(lines) != 1 || lines[0]["level"] != "error" || !strings.HasPrefix(fmt.Sprint(lines[0]["message"]), "cannot read the raft configuration") {
					t.Fatalf("logged %v, want one ERROR line: cannot read the raft configuration", lines)
				}
			})
		}
	}
}

// A change that lost leadership while it committed, or whose node shut down
// before its outcome was known, can still take effect: it logs one WARN line
// that its outcome is unknown, which names both causes, with who changed what,
// the fields of the INFO line of a change, and no such INFO line. A change that
// failed otherwise, refused for a configuration that changed, logs none.
func TestAChangeOfUnknownOutcomeIsLogged(t *testing.T) {
	lost := fmt.Errorf("raft: %w", cluster.ErrLeadershipLost)
	changed := fmt.Errorf("raft: %w", cluster.ErrConfigurationChanged)
	add := func(svc *Service) error {
		return svc.AddMember(context.Background(), admin, AddRequest{ID: "n5", Address: "10.0.0.5:7000"})
	}
	remove := func(svc *Service) error {
		return svc.RemoveMember(context.Background(), admin, RemoveRequest{ID: "n4", Force: true})
	}
	for _, tc := range []struct {
		name   string
		setup  func(*stubAdmin)
		change func(*Service) error
		want   error
		line   map[string]any // the WARN line, nil for none
	}{
		{
			"an addition that lost leadership", func(s *stubAdmin) { s.addErr = lost }, add, cluster.ErrLeadershipLost,
			map[string]any{"actor_user_id": "user-admin", "operation": "add", "member_id": "n5", "member_address": "10.0.0.5:7000", "force": false},
		},
		{
			"a removal that lost leadership", func(s *stubAdmin) { s.removeErr = lost }, remove, cluster.ErrLeadershipLost,
			map[string]any{"actor_user_id": "user-admin", "operation": "remove", "member_id": "n4", "member_address": "10.0.0.4:7000", "force": true},
		},
		{"a removal refused for a configuration that changed", func(s *stubAdmin) { s.removeErr = changed }, remove, cluster.ErrConfigurationChanged, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raft := leading("n1", "n2", "n3", "n4")
			tc.setup(raft)
			var logs bytes.Buffer
			if err := tc.change(newService(raft, zerolog.New(&logs))); !errors.Is(err, tc.want) {
				t.Fatalf("%v, want %v", err, tc.want)
			}
			lines := logLines(t, &logs)
			if tc.line == nil {
				if len(lines) != 0 {
					t.Fatalf("logged %v, want nothing", lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("logged %v, want one WARN line of an outcome unknown", lines)
			}
			if message := fmt.Sprint(lines[0]["message"]); lines[0]["level"] != "warn" ||
				!strings.Contains(message, "outcome unknown") || !strings.Contains(message, "lost its leadership") || !strings.Contains(message, "shut down") {
				t.Fatalf("logged %v, want one WARN line of an outcome unknown, after a lost leadership or a shutdown", lines)
			}
			for key, value := range tc.line {
				if lines[0][key] != value {
					t.Fatalf("the WARN line has %s = %v, want %v: %v", key, lines[0][key], value, lines[0])
				}
			}
			for _, secret := range []string{admin.TokenID, admin.SessionID} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("the log names %q: %s", secret, logs.String())
				}
			}
		})
	}
}

// The status is the node's own view: its ID and role, the leader it knows, the
// servers of its latest configuration, the last index of its log, and the
// fingerprint of its settings that every replica must share.
func TestStatusIsTheNodesView(t *testing.T) {
	const fingerprint = "6a1f7c0e9d2b4f8a6a1f7c0e9d2b4f8a6a1f7c0e9d2b4f8a6a1f7c0e9d2b4f8a"
	raft := leading("n1", "n2", "n3")
	raft.role, raft.leader = cluster.RoleFollower, cluster.Server{ID: "n2", Address: "10.0.0.2:7000"}
	got := New(raft, zerolog.Nop(), WithFingerprint(fingerprint)).Status(context.Background())
	want := Status{NodeID: "n1", Role: cluster.RoleFollower, LeaderID: "n2", LeaderAddr: "10.0.0.2:7000", Members: raft.conf.Servers, LastIndex: 42, Fingerprint: fingerprint}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Status() = %+v, want %+v", got, want)
	}
}
