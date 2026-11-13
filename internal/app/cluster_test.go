package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
	clustersvc "github.com/soltiHQ/control-plane/internal/service/cluster"
)

// operator is the actor of the membership changes of these tests.
var operator = &identity.Identity{UserID: "user-admin"}

// memberConfigs returns the raft configurations of the members, which bootstrap
// one cluster that lists them all, and then of the joiners, which share the
// members' CA, list no peer and join: the leader adds them.
func memberConfigs(t *testing.T, members []string, joiners ...string) []cluster.Config {
	t.Helper()
	cfgs := raftConfigs(t, append(slices.Clone(members), joiners...)...)
	peers := make([]string, 0, len(members))
	for _, cfg := range cfgs[:len(members)] {
		peers = append(peers, cfg.Raft.NodeID+"="+cfg.Raft.BindAddr)
	}
	for i := range cfgs {
		if i < len(members) {
			cfgs[i].Discovery.Peers = peers
			continue
		}
		cfgs[i].Raft.Join, cfgs[i].Discovery.Peers = true, nil
	}
	return cfgs
}

// clusterOf is the cluster service of n, as New builds it.
func clusterOf(n *raftNode) *clustersvc.Service { return clustersvc.New(n.admin, zerolog.Nop()) }

// memberIDs returns the IDs of the servers of n's latest configuration.
func memberIDs(n *raftNode) []string {
	var ids []string
	for _, s := range n.admin.Servers() {
		ids = append(ids, s.ID)
	}
	return ids
}

// holds reports whether n holds the agent id.
func holds(n *raftNode, id string) bool {
	_, err := n.store.GetAgent(context.Background(), id)
	return err == nil
}

// putAgent writes the agent id through the leader among nodes, again through
// the next leader when leadership moves first; the write is an upsert, so one
// whose leadership was lost while it committed is safe to repeat.
func putAgent(t *testing.T, nodes []*raftNode, id string) {
	t.Helper()
	agent, err := model.NewAgent(id, id, "http://agent")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		leader, _ := readyLeader(t, nodes)
		err := leader.store.UpsertAgent(context.Background(), agent)
		switch {
		case err == nil:
			return
		case !cluster.IsLeadershipError(err) || time.Now().After(deadline):
			t.Fatalf("write %s: %v", id, err)
		}
	}
}

// A dead member is replaced through the membership API, by a removal and an
// addition, and the cluster keeps its quorum and its state. Of three nodes n3
// dies; the leader removes it, which leaves two voters and so needs force, and
// adds n4, whose data directory is empty and which starts in join mode, so it
// bootstraps nothing. n4 catches up the state written before, writes keep
// committing, and every member holds all of it. The nodes are real raft nodes
// with mutual TLS, started by buildCluster, and the changes go through the
// leader's cluster service.
func TestReplaceDeadMember(t *testing.T) {
	cfgs := memberConfigs(t, []string{"n1", "n2", "n3"}, "n4")
	var nodes []*raftNode
	for _, cfg := range cfgs[:3] {
		nodes = append(nodes, startRaftNode(t, cfg))
	}
	putAgent(t, nodes, "written-before")
	for _, n := range nodes {
		waitFor(t, 5*time.Second, n.id+" holds the state written before", func() bool { return holds(n, "written-before") })
	}

	dead, survivors := nodes[2], nodes[:2]
	dead.stop()
	leader, _ := readyLeader(t, survivors)
	svc := clusterOf(leader)
	ctx := context.Background()
	if err := svc.RemoveMember(ctx, operator, clustersvc.RemoveRequest{ID: dead.id}); !errors.Is(err, clustersvc.ErrForceRequired) {
		t.Fatalf("remove %s without force: %v, want %v", dead.id, err, clustersvc.ErrForceRequired)
	}
	if err := svc.RemoveMember(ctx, operator, clustersvc.RemoveRequest{ID: dead.id, Force: true}); err != nil {
		t.Fatalf("remove %s with force: %v", dead.id, err)
	}

	joiner := startRaftNode(t, cfgs[3])
	if err := svc.AddMember(ctx, operator, clustersvc.AddRequest{ID: joiner.id, Address: joiner.addr}); err != nil {
		t.Fatalf("add %s: %v", joiner.id, err)
	}
	members := append(slices.Clone(survivors), joiner)
	waitFor(t, 10*time.Second, joiner.id+" catches up the state written before it joined", func() bool {
		return holds(joiner, "written-before")
	})
	putAgent(t, members, "written-after")
	want := []string{"n1", "n2", "n4"}
	for _, n := range members {
		waitFor(t, 5*time.Second, n.id+" holds the state written before and after, in the configuration "+strings.Join(want, ", "), func() bool {
			return holds(n, "written-before") && holds(n, "written-after") && slices.Equal(memberIDs(n), want)
		})
	}
}

// The leader asked to remove itself hands its leadership over before anything
// is removed: the call moves the leadership to another voter and answers that
// it moved, while every node still lists the old leader as a member. Repeated
// at the new leader, the call removes the old leader, a follower by then: it
// leaves every member's configuration, and its raft is not shut down and never
// logged "removed ourself", as hashicorp/raft does when a leader commits its own
// removal. Writes commit without it. The nodes are real raft nodes with mutual
// TLS, started by buildCluster, and the calls go through the cluster services.
func TestRemoveLeaderTransfersFirst(t *testing.T) {
	logs := map[string]*syncBuffer{}
	var nodes []*raftNode
	for _, cfg := range raftConfigs(t, "n1", "n2", "n3") {
		logs[cfg.Raft.NodeID] = &syncBuffer{}
		nodes = append(nodes, startRaftNodeLogging(t, cfg, zerolog.New(logs[cfg.Raft.NodeID])))
	}
	old, _ := readyLeader(t, nodes)
	ctx := context.Background()
	remove := clustersvc.RemoveRequest{ID: old.id, Force: true}

	if err := clusterOf(old).RemoveMember(ctx, operator, remove); !errors.Is(err, clustersvc.ErrLeadershipMoved) {
		t.Fatalf("remove the leader %s: %v, want %v", old.id, err, clustersvc.ErrLeadershipMoved)
	}
	if old.leadership.AmLeader() {
		t.Fatalf("%s still leads after its removal handed its leadership over", old.id)
	}
	if id, _ := old.admin.Leader(); id == "" || id == old.id {
		t.Fatalf("%s answered knowing the leader %q, want the new one", old.id, id)
	}
	next, _ := readyLeader(t, nodes)
	for _, n := range nodes {
		if !slices.Contains(memberIDs(n), old.id) {
			t.Fatalf("%s no longer lists %s when the leadership moved: the removal came first", n.id, old.id)
		}
	}

	if err := clusterOf(next).RemoveMember(ctx, operator, remove); err != nil {
		t.Fatalf("remove %s at the new leader %s: %v", old.id, next.id, err)
	}
	var members []*raftNode
	for _, n := range nodes {
		if n != old {
			members = append(members, n)
		}
	}
	for _, n := range members {
		waitFor(t, 5*time.Second, n.id+" no longer lists "+old.id, func() bool {
			ids := memberIDs(n)
			return len(ids) == 2 && !slices.Contains(ids, old.id)
		})
	}
	if role := old.admin.Role(); role == cluster.RoleShutdown {
		t.Fatalf("the raft of %s shut down: it committed its own removal as the leader", old.id)
	}
	if strings.Contains(logs[old.id].String(), "removed ourself") {
		t.Fatalf("%s logged that it removed itself as the leader", old.id)
	}
	putAgent(t, members, "written-without-the-old-leader")
	for _, n := range members {
		waitFor(t, 5*time.Second, n.id+" holds the write", func() bool { return holds(n, "written-without-the-old-leader") })
	}
}

// changedAfterRead is the cluster administration of a node whose configuration
// changes, by change, right after the cluster service read it and before its
// own change is proposed: a concurrent change, made deterministic.
type changedAfterRead struct {
	cluster.Admin
	change func()
	once   sync.Once
}

func (a *changedAfterRead) Configuration() (cluster.Configuration, error) {
	conf, err := a.Admin.Configuration()
	a.once.Do(a.change)
	return conf, err
}

// On a real cluster the changes that the rules refuse change nothing: an added
// ID that a member has, an address at which a member answers, as written or
// under another name, localhost, which the member's certificate names as well,
// so that the probe alone would let it in as a second voter on the member's
// node, an address where no node answers, and a removal that needs force. Nor
// does a change whose configuration changed between the read of its rules and
// its proposal: hashicorp/raft refuses it by the index the rules read, which the
// API answers 409 "configuration changed, retry". After each, the configuration
// is the one before it, at its index.
func TestRefusedChangesChangeNothing(t *testing.T) {
	leader, followers := raftCluster(t, "n1", "n2", "n3")
	svc := clusterOf(leader)
	ctx := context.Background()
	before, err := leader.admin.Configuration()
	if err != nil {
		t.Fatal(err)
	}
	closed := loopbackAddrs(t, 1)[0]
	_, port, err := net.SplitHostPort(followers[0].addr)
	if err != nil {
		t.Fatal(err)
	}
	alias := net.JoinHostPort("localhost", port)
	if err := leader.admin.Probe(ctx, alias); err != nil {
		t.Fatalf("probe of %s, another name of %s: %v, want the member to answer there", alias, followers[0].id, err)
	}
	for _, tc := range []struct {
		name   string
		change func() error
		want   error
		holder string // the member an AddressInUseError names
	}{
		{"the ID of a member", func() error {
			return svc.AddMember(ctx, operator, clustersvc.AddRequest{ID: followers[0].id, Address: closed})
		}, clustersvc.ErrMemberExists, ""},
		{"the address of a member", func() error {
			return svc.AddMember(ctx, operator, clustersvc.AddRequest{ID: "n9", Address: followers[0].addr})
		}, clustersvc.ErrAddressInUse, followers[0].id},
		{"another name of a member", func() error {
			return svc.AddMember(ctx, operator, clustersvc.AddRequest{ID: "n9", Address: alias})
		}, clustersvc.ErrAddressInUse, followers[0].id},
		{"an address where no node answers", func() error {
			return svc.AddMember(ctx, operator, clustersvc.AddRequest{ID: "n9", Address: closed})
		}, cluster.ErrPeerUnreachable, ""},
		{"a removal that needs force", func() error {
			return svc.RemoveMember(ctx, operator, clustersvc.RemoveRequest{ID: followers[0].id})
		}, clustersvc.ErrForceRequired, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.change()
			if !errors.Is(err, tc.want) {
				t.Fatalf("%v, want %v", err, tc.want)
			}
			var inUse *clustersvc.AddressInUseError
			if tc.holder != "" && (!errors.As(err, &inUse) || inUse.Member != tc.holder) {
				t.Fatalf("%v, want the member %s named", err, tc.holder)
			}
			after, err := leader.admin.Configuration()
			if err != nil || after.Index != before.Index || !slices.Equal(after.Servers, before.Servers) {
				t.Fatalf("the configuration became %+v (%v), want %+v", after, err, before)
			}
		})
	}

	t.Run("a configuration that changed after the read", func(t *testing.T) {
		gone, kept := followers[1], followers[0]
		var raced uint64
		racing := &changedAfterRead{Admin: leader.admin, change: func() {
			var err error
			if raced, err = leader.admin.RemoveServer(gone.id, before.Index); err != nil {
				t.Errorf("the concurrent removal of %s: %v", gone.id, err)
			}
		}}
		err := clustersvc.New(racing, zerolog.Nop()).RemoveMember(ctx, operator, clustersvc.RemoveRequest{ID: kept.id, Force: true})
		if !errors.Is(err, cluster.ErrConfigurationChanged) {
			t.Fatalf("remove %s after the configuration changed: %v, want %v", kept.id, err, cluster.ErrConfigurationChanged)
		}
		after, err := leader.admin.Configuration()
		if err != nil || after.Index != raced || !slices.Contains(memberIDs(leader), kept.id) || slices.Contains(memberIDs(leader), gone.id) {
			t.Fatalf("the configuration became %+v (%v), want the concurrent change's alone, at index %d", after, err, raced)
		}
	})
}

// Memory registers no cluster route: its cluster paths answer as before
// raft mode had them, as every path the UI's catch-all page route takes. An
// authenticated GET is 404, an authenticated POST or DELETE 405, as that route
// answers a method other than GET, and a request without credentials 401.
func TestClusterRoutesAbsentInMemory(t *testing.T) {
	f := newBrowserFixture(t, config.Default(), nil)
	cookies := f.login(t, "alice")
	for _, tc := range []struct {
		method, target        string
		authenticated, absent int
	}{
		{http.MethodGet, "/api/v1/cluster", http.StatusNotFound, http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/cluster/members", http.StatusMethodNotAllowed, http.StatusUnauthorized},
		{http.MethodDelete, "/api/v1/cluster/members/n1", http.StatusMethodNotAllowed, http.StatusUnauthorized},
	} {
		for _, authenticated := range []bool{true, false} {
			r := httptest.NewRequest(tc.method, tc.target, strings.NewReader(`{"id":"n4","address":"127.0.0.1:7004"}`))
			r.Header.Set("Accept", jsonType)
			r.Header.Set("Content-Type", jsonType)
			want := tc.absent
			if authenticated {
				for _, c := range cookies {
					r.AddCookie(c)
				}
				want = tc.authenticated
			}
			if w := f.serve(r); w.Code != want {
				t.Errorf("%s %s, authenticated %v: HTTP %d %s, want %d", tc.method, tc.target, authenticated, w.Code, w.Body.String(), want)
			}
		}
	}
}
