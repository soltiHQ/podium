package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/httpcookie"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// loopbackIPs returns count loopback IPs, 127.0.0.1 and on, one for each whole
// node of a test, so that the nodes can serve every listener on one port, as
// the same-port assumption wants. Linux gives every host 127.0.0.0/8 as
// loopback: the test skips on another system, and fails on Linux when an IP
// cannot be bound, so that the gate, which runs on Linux, never loses it.
func loopbackIPs(t *testing.T, count int) []string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("each node needs a loopback IP of its own, which Linux gives every host (127.0.0.0/8) and %s may not", runtime.GOOS)
	}
	hosts := []string{"127.0.0.1"}
	for i := 2; len(hosts) < count; i++ {
		host := net.IPv4(127, 0, 0, byte(i)).String()
		l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			t.Fatalf("each node needs a loopback IP of its own, and %s cannot be bound on Linux: %v", host, err)
		}
		_ = l.Close()
		hosts = append(hosts, host)
	}
	return hosts
}

// wholeNodesAt returns one whole node per ID, none started, node i on the
// loopback IP hosts[i], all serving raft, HTTP, HTTP discovery and gRPC on one
// port per listener, and listing every node of ids as its peers.
func wholeNodesAt(t *testing.T, hosts []string, ids ...string) []*clusterNode {
	t.Helper()
	ports := sharedPorts(t, hosts, 4) // raft, HTTP, HTTP discovery and gRPC
	raftAddrs := make([]string, 0, len(hosts))
	for _, host := range hosts {
		raftAddrs = append(raftAddrs, net.JoinHostPort(host, ports[0]))
	}
	nodes := clusterNodesOf(t, raftConfigsAt(t, raftAddrs, ids...))
	for i, n := range nodes {
		n.cfg.HTTP.Addr = net.JoinHostPort(hosts[i], ports[1])
		n.cfg.HTTPDiscovery.Addr = net.JoinHostPort(hosts[i], ports[2])
		n.cfg.GRPC.Addr = net.JoinHostPort(hosts[i], ports[3])
	}
	return nodes
}

// serving waits until each of nodes, started, serves its three listeners: Run
// serves them once New has returned.
func serving(t *testing.T, nodes ...*clusterNode) {
	t.Helper()
	for _, n := range nodes {
		for _, addr := range []string{n.cfg.HTTP.Addr, n.cfg.HTTPDiscovery.Addr, n.cfg.GRPC.Addr} {
			waitFor(t, 10*time.Second, n.id+" serves "+addr, func() bool {
				conn, err := net.Dial("tcp", addr)
				if err == nil {
					_ = conn.Close()
				}
				return err == nil
			})
		}
	}
}

// warnedOfPlaintextForwarding fails the test unless each of nodes, run without
// tls.server, logged the WARN of its plaintext forwarding once at its start.
func warnedOfPlaintextForwarding(t *testing.T, nodes ...*clusterNode) {
	t.Helper()
	for _, n := range nodes {
		if got := n.logged(t, map[string]any{"level": "warn", "message": plaintextForwardingWarning}); len(got) != 1 {
			t.Fatalf("%s logged the WARN of plaintext forwarding %d times, want once", n.id, len(got))
		}
	}
}

// httpClient follows no redirect, so that a test reads each answer itself.
func httpClient() *http.Client {
	return &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// send sends a request with method to target, with body of contentType when
// body is set, token as its bearer token when set, and cookies, and returns the
// answer with its body read.
func send(t *testing.T, client *http.Client, method, target, contentType, body, token string, cookies []*http.Cookie) (*http.Response, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w, err := client.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = w.Body.Close() }()
	answer, err := io.ReadAll(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	return w, string(answer)
}

// loginThrough creates on leader's store a user, subject, who holds perms, with
// a cheap hash, logs it in through via, a follower that forwards the login to
// the leader, and returns its cookies once every one of nodes holds the
// session of the login, so that each checks the cookies' access token against
// its own store.
func loginThrough(t *testing.T, leader, via *clusterNode, nodes []*clusterNode, subject string, perms ...enum.Permission) []*http.Cookie {
	t.Helper()
	ctx := context.Background()
	u, err := model.NewUser("user-"+subject, subject)
	if err != nil {
		t.Fatal(err)
	}
	granted := make([]string, 0, len(perms))
	for _, p := range perms {
		granted = append(granted, string(p))
	}
	u.SetPermissions(granted)
	if err := leader.app.store.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := credential.New(leader.app.store, zerolog.Nop()).SetPassword(ctx, credential.SetPasswordRequest{UserID: u.ID(), Password: browserPassword, Cost: 4}); err != nil {
		t.Fatal(err)
	}
	login, body := send(t, httpClient(), http.MethodPost, "http://"+via.cfg.HTTP.Addr+"/login", formType, loginForm(subject), "", nil)
	if login.StatusCode != http.StatusFound {
		t.Fatalf("login of %s through %s: %d %q, want the leader's 302", subject, via.id, login.StatusCode, body)
	}
	var session string
	for _, c := range login.Cookies() {
		if c.Name == httpcookie.NameSessionID {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatalf("the login of %s through %s set no %s cookie", subject, via.id, httpcookie.NameSessionID)
	}
	nodesHold(t, nodes, "the session of "+subject, func(s storage.Storage) bool {
		_, err := s.GetSession(ctx, session)
		return err == nil
	})
	return login.Cookies()
}

// nodesHold waits until every one of nodes holds what found finds in its store.
func nodesHold(t *testing.T, nodes []*clusterNode, what string, found func(storage.Storage) bool) {
	t.Helper()
	for _, n := range nodes {
		waitFor(t, 10*time.Second, n.id+" holds "+what, func() bool { return found(n.app.store) })
	}
}

// servedAPI reports whether n's access log holds a request of method to path
// answered with status: the access log of an API route runs behind the Leader
// middleware, so only the node that served a request logs it.
func servedAPI(t *testing.T, n *clusterNode, method, path string, status int) bool {
	t.Helper()
	return len(n.logged(t, map[string]any{"message": "http request", "method": method, "path": path, "status": float64(status)})) > 0
}

// Three whole nodes run as main runs them, each on a loopback IP of its own
// with one port per listener. Before the failover, each follower forwards an
// agent's gRPC sync to the leader, so that the Forwarder of each holds a client
// connection to the leader's gRPC address, and a user logs in through a
// follower. The leader then stops, and the other two elect a new one. Writes
// sent to the follower that remains then land on the new leader, and both
// survivors hold them: a gRPC sync, which its Forwarder sends to the new
// leader's address in place of the dead one's, an HTTP discovery sync and the
// creation of a user on the main listener, which the Leader middleware sends to
// the leader it knows at each request, with the cookies of the login made
// before the failover. The new leader's access log holds the creation, the
// follower's does not, and the follower follows the new leader once each write
// is answered. Every node, which runs without tls.server, logged the WARN of
// its plaintext forwarding once at start.
func TestFollowerWritesLandOnTheNewLeaderAfterFailover(t *testing.T) {
	nodes := wholeNodesAt(t, loopbackIPs(t, 3), "n1", "n2", "n3")
	for _, n := range nodes {
		n.start(t)
	}
	serving(t, nodes...)
	warnedOfPlaintextForwarding(t, nodes...)
	leader, followers := readyNodes(t, nodes)
	ctx := context.Background()
	agentHeld := func(id string) func(storage.Storage) bool {
		return func(s storage.Storage) bool {
			_, err := s.GetAgent(ctx, id)
			return err == nil
		}
	}

	for _, via := range followers {
		id := "agent-before-through-" + via.id
		resp, err := discoveryClient(t, via.cfg.GRPC.Addr, nil, nil).Sync(withToken(ctx, "token-of-"+id), syncRequest(id))
		if err != nil || !resp.GetSuccess() {
			t.Fatalf("sync through %s before the failover: %v %v, want the leader's success", via.id, resp, err)
		}
		nodesHold(t, nodes, id, agentHeld(id))
	}
	cookies := loginThrough(t, leader, followers[0], nodes, "writer", enum.UsersGet, enum.UsersAdd)

	leader.stop()
	next, rest := readyNodes(t, followers)
	via := rest[0]
	survivors := []*clusterNode{next, via}
	// follows fails the test unless via follows next once what was answered: a
	// node that led meanwhile may have served it itself.
	follows := func(what string) {
		t.Helper()
		if via.app.leadership.AmLeader() || via.app.leadership.CurrentLeader() != next.cfg.Cluster.Raft.BindAddr {
			t.Fatalf("%s no longer follows %s once %s was answered", via.id, next.id, what)
		}
	}

	resp, err := discoveryClient(t, via.cfg.GRPC.Addr, nil, nil).Sync(withToken(ctx, "token-of-agent-after-grpc"), syncRequest("agent-after-grpc"))
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("sync through %s over gRPC after the failover: %v %v, want %s's success", via.id, resp, err, next.id)
	}
	follows("the gRPC sync")
	nodesHold(t, survivors, "the agent synced over gRPC after the failover", agentHeld("agent-after-grpc"))

	client := httpClient()
	httpSync, body := send(t, client, http.MethodPost, "http://"+via.cfg.HTTPDiscovery.Addr+"/api/v1/discovery/sync", jsonType,
		`{"id":"agent-after-http","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`, "token-of-agent-after-http", nil)
	if httpSync.StatusCode != http.StatusOK {
		t.Fatalf("sync through %s over HTTP after the failover: %d %q, want %s's 200", via.id, httpSync.StatusCode, body, next.id)
	}
	follows("the HTTP sync")
	nodesHold(t, survivors, "the agent synced over HTTP after the failover", agentHeld("agent-after-http"))

	created, body := send(t, client, http.MethodPost, "http://"+via.cfg.HTTP.Addr+"/api/v1/users", jsonType, `{"subject":"after-the-failover"}`, "", cookies)
	if created.StatusCode != http.StatusNoContent {
		t.Fatalf("user created through %s after the failover: %d %q, want %s's 204", via.id, created.StatusCode, body, next.id)
	}
	follows("the creation of a user")
	nodesHold(t, survivors, "the user created after the failover", func(s storage.Storage) bool {
		_, err := s.GetUserBySubject(ctx, "after-the-failover")
		return err == nil
	})
	waitFor(t, 5*time.Second, next.id+" logs the creation of the user", func() bool {
		return servedAPI(t, next, http.MethodPost, "/api/v1/users", http.StatusNoContent)
	})
	if servedAPI(t, via, http.MethodPost, "/api/v1/users", http.StatusNoContent) {
		t.Fatalf("%s served the creation of the user itself", via.id)
	}
}

// statusMembers returns the IDs of the members of n's configuration, as its cluster
// status gives them.
func statusMembers(n *clusterNode) []string {
	var ids []string
	for _, m := range n.app.cluster.Status(context.Background()).Members {
		ids = append(ids, m.ID)
	}
	return ids
}

// The membership API through a follower, on whole nodes that run as main runs
// them, each on a loopback IP of its own with one port per listener: the
// follower forwards POST and DELETE /api/v1/cluster/members to the leader,
// which applies them. A fourth node started with join: true is added through
// one follower, catches up the state written before, and is removed through
// the other. The leader's own removal, sent to a follower, takes two calls: the
// first answers 503 with X-Leader, the HTTP address of the node the leadership
// moved to, having removed nothing, and the repeat, sent to a follower of the
// new leader, removes the old leader from every remaining member's
// configuration. The leader's access log, not the follower's, holds each change
// it served.
func TestMembershipChangesThroughAFollower(t *testing.T) {
	nodes := wholeNodesAt(t, loopbackIPs(t, 4), "n1", "n2", "n3", "n4")
	members, joiner := nodes[:3], nodes[3]
	peers := members[0].cfg.Cluster.Discovery.Peers[:3]
	for _, n := range members {
		n.cfg.Cluster.Discovery.Peers = peers
	}
	joiner.cfg.Cluster.Discovery.Peers = peers
	joiner.cfg.Cluster.Raft.Join = true
	for _, n := range members {
		n.start(t)
	}
	serving(t, members...)
	leader, followers := readyNodes(t, members)
	ctx := context.Background()
	cookies := loginThrough(t, leader, followers[0], members, "operator", enum.ClusterGet, enum.ClusterManage)
	client := httpClient()
	const path = "/api/v1/cluster/members"
	// lists waits until every one of nodes holds the configuration whose members
	// are want, in order.
	lists := func(nodes []*clusterNode, want ...string) {
		t.Helper()
		for _, n := range nodes {
			waitFor(t, 10*time.Second, n.id+" lists the members "+strings.Join(want, ", "), func() bool {
				return slices.Equal(statusMembers(n), want)
			})
		}
	}

	joiner.start(t)
	serving(t, joiner)
	added, body := send(t, client, http.MethodPost, "http://"+followers[0].cfg.HTTP.Addr+path, jsonType,
		`{"id":"n4","address":"`+joiner.cfg.Cluster.Raft.BindAddr+`"}`, "", cookies)
	if added.StatusCode != http.StatusNoContent {
		t.Fatalf("n4 added through %s: %d %q, want the leader's 204", followers[0].id, added.StatusCode, body)
	}
	lists(append(slices.Clone(members), joiner), "n1", "n2", "n3", "n4")
	nodesHold(t, []*clusterNode{joiner}, "the operator, written before it joined", func(s storage.Storage) bool {
		_, err := s.GetUserBySubject(ctx, "operator")
		return err == nil
	})
	waitFor(t, 5*time.Second, leader.id+" logs the addition", func() bool {
		return servedAPI(t, leader, http.MethodPost, path, http.StatusNoContent)
	})
	if servedAPI(t, followers[0], http.MethodPost, path, http.StatusNoContent) {
		t.Fatalf("%s served the addition itself", followers[0].id)
	}

	removed, body := send(t, client, http.MethodDelete, "http://"+followers[1].cfg.HTTP.Addr+path+"/n4", "", "", "", cookies)
	if removed.StatusCode != http.StatusNoContent {
		t.Fatalf("n4 removed through %s: %d %q, want the leader's 204", followers[1].id, removed.StatusCode, body)
	}
	lists(members, "n1", "n2", "n3")
	joiner.stop()

	// The leader's removal: the first call hands its leadership over.
	via := followers[0]
	moved, body := send(t, client, http.MethodDelete, "http://"+via.cfg.HTTP.Addr+path+"/"+leader.id+"?force=true", "", "", "", cookies)
	var answer struct{ Message string }
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("the first removal of %s answered %q: %v", leader.id, body, err)
	}
	var next *clusterNode
	for _, n := range followers {
		if n.cfg.HTTP.Addr == moved.Header.Get("X-Leader") {
			next = n
		}
	}
	if moved.StatusCode != http.StatusServiceUnavailable || next == nil || !strings.Contains(answer.Message, "repeat the request") {
		t.Fatalf("the first removal of %s through %s: %d with X-Leader %q and %q, want 503 naming the HTTP address of the new leader, and to repeat the request",
			leader.id, via.id, moved.StatusCode, moved.Header.Get("X-Leader"), answer.Message)
	}
	for _, n := range members {
		if ids := statusMembers(n); !slices.Contains(ids, leader.id) {
			t.Fatalf("%s lists %v after the first call: the hand-over removed %s", n.id, ids, leader.id)
		}
	}
	if got, _ := readyNodes(t, members); got != next {
		t.Fatalf("%s leads, not %s, which the 503 named", got.id, next.id)
	}

	// The repeat, through the follower of the new leader that is not the old one.
	var third *clusterNode
	for _, n := range followers {
		if n != next {
			third = n
		}
	}
	gone, body := send(t, client, http.MethodDelete, "http://"+third.cfg.HTTP.Addr+path+"/"+leader.id+"?force=true", "", "", "", cookies)
	if gone.StatusCode != http.StatusNoContent {
		t.Fatalf("the repeated removal of %s through %s: %d %q, want %s's 204", leader.id, third.id, gone.StatusCode, body, next.id)
	}
	var want []string
	for _, id := range []string{"n1", "n2", "n3"} {
		if id != leader.id {
			want = append(want, id)
		}
	}
	lists([]*clusterNode{next, third}, want...)
	waitFor(t, 5*time.Second, next.id+" logs the repeated removal", func() bool {
		return servedAPI(t, next, http.MethodDelete, path+"/"+leader.id, http.StatusNoContent)
	})
	if servedAPI(t, third, http.MethodDelete, path+"/"+leader.id, http.StatusNoContent) {
		t.Fatalf("%s served the repeated removal itself", third.id)
	}
}
