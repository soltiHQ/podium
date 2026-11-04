package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/server/runner/lifecycle"
	"github.com/soltiHQ/control-plane/internal/storage"
)

const (
	// passwordGenerated is what bootstrap logs once it stored a password it
	// generated and wrote to the password file.
	passwordGenerated = "bootstrap: admin password generated: read it from the file, log in and change it"
	// termTaskDone is what a runner that works once per leadership term logs
	// when its work of the term is done.
	termTaskDone = "runner task done for this leadership term"
	// newStartLimit bounds how long New may take to start a raft node: it
	// starts the node and waits for nothing, a leader least of all.
	newStartLimit = 5 * time.Second
)

// clusterNode is a node of a raft cluster as main runs it: New builds it from
// its configuration, and Run runs it until stop, or the end of the test.
type clusterNode struct {
	id   string
	cfg  config.Config
	logs *syncBuffer // what the node logs, one JSON object per line
	app  *App        // nil until the node starts
	stop func()
}

// clusterNodes returns one node per ID, none of them started yet, each with the
// raft configuration raftConfigs gives it and its listeners on loopback ports
// that the kernel picks when the node runs.
func clusterNodes(t *testing.T, ids ...string) []*clusterNode {
	t.Helper()
	nodes := make([]*clusterNode, 0, len(ids))
	for _, raft := range raftConfigs(t, ids...) {
		cfg := config.Default()
		cfg.Cluster = raft
		cfg.Auth.JWTSecret = strings.Repeat("leadership-term-test-secret", 2)
		cfg.HTTP.Addr, cfg.HTTPDiscovery.Addr, cfg.GRPC.Addr = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
		nodes = append(nodes, &clusterNode{id: raft.Raft.NodeID, cfg: cfg, logs: &syncBuffer{}})
	}
	return nodes
}

// start builds n with New and runs it, as main does. New must return within
// newStartLimit, whether a leader is known or not. n stops when the test ends,
// if stop did not stop it first.
func (n *clusterNode) start(t *testing.T) {
	t.Helper()
	begin := time.Now()
	a, err := New(context.Background(), n.cfg, zerolog.New(n.logs))
	if err != nil {
		t.Fatalf("start %s: %v", n.id, err)
	}
	took := time.Since(begin)
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- a.Run(ctx) }()
	n.app = a
	n.stop = sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-ran:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s: Run ended with %v before it was stopped", n.id, err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("%s: Run did not return within 10s of its cancellation", n.id)
		}
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		if err := a.Shutdown(shutdownCtx); err != nil {
			t.Errorf("%s: shutdown: %v", n.id, err)
		}
	})
	t.Cleanup(n.stop)
	if took > newStartLimit {
		t.Fatalf("New took %s to start %s: it must not wait for a leader", took, n.id)
	}
}

// logged returns the lines n logged that hold every field of want.
func (n *clusterNode) logged(t *testing.T, want map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(n.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("%s logged %q: %v", n.id, line, err)
		}
		matches := true
		for key, value := range want {
			matches = matches && entry[key] == value
		}
		if matches {
			out = append(out, entry)
		}
	}
	return out
}

// passwordFilePath is where bootstrap writes the password it generates on n.
func (n *clusterNode) passwordFilePath() string {
	return filepath.Join(n.cfg.Cluster.Raft.DataDir, bootstrap.PasswordFile)
}

// passwordFile returns the password in n's password file, and whether n has one.
func (n *clusterNode) passwordFile(t *testing.T) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(n.passwordFilePath())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false
	case err != nil:
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b)), true
}

// seededAdmin describes the administrator that store holds. It returns the
// stored password hash when store holds exactly one user, user-admin, with
// exactly one password credential; otherwise what it holds instead.
func seededAdmin(store storage.Storage) (hash string, err error) {
	ctx := context.Background()
	users, err := store.ListUsers(ctx, nil, storage.ListOptions{Limit: 10})
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(users.Items))
	for _, u := range users.Items {
		ids = append(ids, u.ID())
	}
	if len(ids) != 1 || ids[0] != "user-admin" {
		return "", fmt.Errorf("users %v, want only user-admin", ids)
	}
	creds, err := store.ListCredentialsByUser(ctx, "user-admin")
	if err != nil {
		return "", err
	}
	var passwords []*model.Credential
	for _, c := range creds {
		if c.AuthKind() == enum.Password {
			passwords = append(passwords, c)
		}
	}
	if len(passwords) != 1 {
		return "", fmt.Errorf("user-admin has %d password credentials, want one", len(passwords))
	}
	v, err := store.GetVerifierByCredential(ctx, passwords[0].ID())
	if err != nil {
		return "", err
	}
	hash, _ = v.DataGet(credentials.PasswordHashKey)
	return hash, nil
}

// verifiesAdmin reports whether password is the administrator's stored password in store.
func verifiesAdmin(t *testing.T, store storage.Storage, password string) bool {
	t.Helper()
	ctx := context.Background()
	c, err := store.GetCredentialByUserAndAuth(ctx, "user-admin", enum.Password)
	if err != nil {
		t.Fatal(err)
	}
	v, err := store.GetVerifierByCredential(ctx, c.ID())
	if err != nil {
		t.Fatal(err)
	}
	return credentials.VerifyPassword(c, v, password) == nil
}

// waitForSeed waits until every node of nodes holds the administrator and its
// password, and returns the one node that logged a generated password.
func waitForSeed(t *testing.T, nodes []*clusterNode) *clusterNode {
	t.Helper()
	generated := map[string]any{"message": passwordGenerated}
	waitFor(t, 20*time.Second, "a seed that every node holds", func() bool {
		seeded := false
		for _, n := range nodes {
			if _, err := seededAdmin(n.app.store); err != nil {
				return false
			}
			seeded = seeded || len(n.logged(t, generated)) > 0
		}
		return seeded
	})
	var seeders []*clusterNode
	for _, n := range nodes {
		if lines := n.logged(t, generated); len(lines) > 0 {
			if len(lines) > 1 {
				t.Fatalf("%s generated a password %d times, want once", n.id, len(lines))
			}
			seeders = append(seeders, n)
		}
	}
	if len(seeders) != 1 {
		t.Fatalf("%d nodes generated a password, want one", len(seeders))
	}
	return seeders[0]
}

// A node that starts alone has no quorum: New starts it without waiting for a
// leader, and nothing is seeded while no node can lead. The other two nodes
// start several election timeouts later, so no window that opens at a node's
// start could see the quorum form, and the leader of the quorum seeds at its
// leadership gain: exactly one administrator, whose generated password lies in
// the data directory of the node that logged it, named by its node ID and the
// file's absolute path, and on no other node.
func TestLateQuorumStillSeedsOnce(t *testing.T) {
	nodes := clusterNodes(t, "n1", "n2", "n3")
	first := nodes[0]
	first.start(t)

	time.Sleep(8 * raftTimeoutMs * time.Millisecond)
	if first.app.leadership.AmLeader() {
		t.Fatal("a node without a quorum leads")
	}
	if _, err := seededAdmin(first.app.store); err == nil {
		t.Fatal("the administrator was seeded without a quorum")
	}
	if _, ok := first.passwordFile(t); ok {
		t.Fatal("a password file was written without a quorum")
	}

	for _, n := range nodes[1:] {
		n.start(t)
	}
	seeder := waitForSeed(t, nodes)
	line := seeder.logged(t, map[string]any{"message": passwordGenerated})[0]
	if line["node_id"] != seeder.id || line["path"] != seeder.passwordFilePath() {
		t.Fatalf("the generated password line names node %v and path %v, want %s and %s", line["node_id"], line["path"], seeder.id, seeder.passwordFilePath())
	}
	if len(seeder.logged(t, map[string]any{"runner": "bootstrap", "message": "runner acquired leadership"})) == 0 {
		t.Fatalf("%s seeded without a leadership term of its bootstrap runner", seeder.id)
	}
	password, ok := seeder.passwordFile(t)
	if !ok {
		t.Fatalf("%s logged a generated password but has no password file", seeder.id)
	}
	if !verifiesAdmin(t, seeder.app.store, password) {
		t.Fatalf("the password in %s's file does not log in", seeder.id)
	}
	hash, err := seededAdmin(seeder.app.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if got, err := seededAdmin(n.app.store); err != nil || got != hash {
			t.Fatalf("%s holds another administrator password than %s stored (%v)", n.id, seeder.id, err)
		}
		if _, ok := n.passwordFile(t); ok && n != seeder {
			t.Fatalf("%s has a password file, though %s seeded", n.id, seeder.id)
		}
	}
}

// Once the administrator is seeded, a new leader runs bootstrap at the start of
// its own term and changes nothing: one administrator, the same stored
// password, and no password file on the new leader, nor on the third node. The
// seeder's file stays in its data directory.
func TestSeedingIsIdempotentAcrossLeaders(t *testing.T) {
	nodes := clusterNodes(t, "n1", "n2", "n3")
	for _, n := range nodes {
		n.start(t)
	}
	seeder := waitForSeed(t, nodes)
	hash, err := seededAdmin(seeder.app.store)
	if err != nil {
		t.Fatal(err)
	}
	password, ok := seeder.passwordFile(t)
	if !ok {
		t.Fatalf("%s seeded without a password file", seeder.id)
	}
	var others []*clusterNode
	done := map[string]any{"runner": "bootstrap", "message": termTaskDone}
	terms := map[*clusterNode]int{}
	for _, n := range nodes {
		if n != seeder {
			others = append(others, n)
			terms[n] = len(n.logged(t, done))
		}
	}

	// Leadership leaves the seeder.
	seeder.stop()
	var next *clusterNode
	waitFor(t, 15*time.Second, "bootstrap done in a leadership term of another node", func() bool {
		for _, n := range others {
			if len(n.logged(t, done)) > terms[n] {
				next = n
				return true
			}
		}
		return false
	})
	for _, n := range others {
		got, err := seededAdmin(n.app.store)
		if err != nil {
			t.Fatalf("%s after the term of %s: %v", n.id, next.id, err)
		}
		if got != hash {
			t.Fatalf("%s holds another password after the term of %s: the seed ran again", n.id, next.id)
		}
		if _, ok := n.passwordFile(t); ok {
			t.Fatalf("%s has a password file after the term of %s: only the seeder writes one", n.id, next.id)
		}
		if lines := n.logged(t, map[string]any{"message": passwordGenerated}); len(lines) != 0 {
			t.Fatalf("%s generated a password: %v", n.id, lines)
		}
	}
	if got, ok := seeder.passwordFile(t); !ok || got != password {
		t.Fatalf("the password file of %s changed or went away", seeder.id)
	}
}

// A replica that takes leadership counts an agent's silence from the start of
// its own term, not from its process start. Every node runs the lifecycle
// runner, built by lifecycle.New as New builds it, from the same start. The
// agent last synced an hour ago, and delete_after is short. The first leader
// counts the silence from its own term, and stops halfway to deleting the
// agent. Counted from its process start, the next leader would delete the agent
// at most delete_after/2 into its term; it keeps the agent for a full
// delete_after into its term instead, and only then deletes it.
func TestNewLeaderGivesAgentsAGrace(t *testing.T) {
	const (
		heartbeat   = 100 * time.Millisecond
		deleteAfter = 2 * time.Second
	)
	nodes := make([]*raftNode, 0, 3)
	for _, cfg := range raftConfigs(t, "n1", "n2", "n3") {
		nodes = append(nodes, startRaftNode(t, cfg))
	}
	leader, followers := readyLeader(t, nodes)

	ctx := context.Background()
	agent, err := model.NewAgentFrom(model.AgentParams{ID: "agent-silent", Endpoint: "http://127.0.0.1:1", EndpointType: 2, APIVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	lastSeen := time.Now().Add(-time.Hour)
	agent.SetHeartbeatInterval(heartbeat)
	agent.SetLastSeenAt(lastSeen)
	agent.SetStaleAt(lastSeen.Add(heartbeat))
	agent.SetStatus(enum.AgentStatusActive)
	if err = leader.store.UpsertAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	stored := func(n *raftNode) bool {
		_, err := n.store.GetAgent(ctx, agent.ID())
		return err == nil
	}
	for _, n := range nodes {
		waitFor(t, 5*time.Second, n.id+" holds the agent", func() bool { return stored(n) })
	}

	cfg := lifecycle.Config{TickInterval: 20 * time.Millisecond, DeleteAfter: deleteAfter}
	runners := map[*raftNode]*lifecycle.Runner{}
	for _, n := range nodes {
		r, err := lifecycle.New(cfg, zerolog.Nop(), n.store, n.hub, n.leadership)
		if err != nil {
			t.Fatal(err)
		}
		runners[n] = r
		go func() { _ = r.Start(context.Background()) }()
		t.Cleanup(func() { _ = r.Stop(context.Background()) })
	}
	started := time.Now()

	time.Sleep(deleteAfter / 2)
	if !stored(leader) {
		t.Fatalf("%s deleted the agent %s after the runners started, before delete_after", leader.id, time.Since(started))
	}
	// The leader stops as its process would: its runners first, then raft.
	if err = runners[leader].Stop(ctx); err != nil {
		t.Fatal(err)
	}
	leader.stop()
	next, _ := readyLeader(t, followers)
	tookOver := time.Now()

	// Counting from its process start, the next leader would delete the agent
	// delete_after after the runners started: at most delete_after/2 into its term.
	for time.Since(tookOver) < deleteAfter*7/10 {
		if !stored(next) {
			t.Fatalf("%s deleted the agent %s into its term, %s after its runner started: it counted silence from before its term",
				next.id, time.Since(tookOver), time.Since(started))
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, 5*time.Second, next.id+" deletes the agent delete_after into its term", func() bool { return !stored(next) })
}
