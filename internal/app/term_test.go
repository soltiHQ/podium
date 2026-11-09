package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
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
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/server/runner/lifecycle"
	"github.com/soltiHQ/control-plane/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

const (
	// passwordGenerated is what bootstrap logs once it stored a password it
	// generated and wrote to the password file.
	passwordGenerated = "bootstrap: admin password generated: read it from the file, log in and change it"
	// storeFailed begins the error of a seed that failed after it wrote its
	// password file, which the error names next.
	storeFailed = "bootstrap: store the admin password written to "
	// What the bootstrap runner, a term task, logs: at the start of each term it
	// leads, once its work of the term is done, for a failed run it retries, and
	// for a run that the end of its term ended.
	acquiredLeadership = "runner acquired leadership"
	termTaskDone       = "runner task done for this leadership term"
	termTaskRetry      = "runner task failed: retrying"
	termTaskEnded      = "runner task ended with its leadership term: the next term runs it again"
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
	// runDone is closed once Run has returned; runErr, set before it closes, is
	// what Run returned.
	runDone chan struct{}
	runErr  error
	// discarded marks a node of an attempt that startWholeNodes gave up: its Run
	// may have ended on its own, which stop then does not report.
	discarded bool
}

// clusterNodes returns one node per ID, none of them started yet, each with the
// raft configuration raftConfigs gives it and its listeners on loopback ports
// that the kernel picks when the node runs.
func clusterNodes(t *testing.T, ids ...string) []*clusterNode {
	t.Helper()
	return clusterNodesOf(t, raftConfigs(t, ids...))
}

// clusterNodesOf is clusterNodes with the raft configurations raft, one node
// each, whose listeners listen on the host of the node's raft address.
func clusterNodesOf(t *testing.T, raft []cluster.Config) []*clusterNode {
	t.Helper()
	nodes := make([]*clusterNode, 0, len(raft))
	for _, r := range raft {
		cfg := config.Default()
		cfg.Cluster = r
		cfg.Auth.JWTSecret = strings.Repeat("leadership-term-test-secret", 2)
		host, _, err := net.SplitHostPort(r.Raft.BindAddr)
		if err != nil {
			t.Fatal(err)
		}
		listen := net.JoinHostPort(host, "0")
		cfg.HTTP.Addr, cfg.HTTPDiscovery.Addr, cfg.GRPC.Addr, cfg.Ops.Addr = listen, listen, listen, listen
		nodes = append(nodes, &clusterNode{id: r.Raft.NodeID, cfg: cfg, logs: &syncBuffer{}})
	}
	return nodes
}

// start builds n with New and runs it, as main does. New must return within
// newStartLimit, whether a leader is known or not. n stops when the test ends,
// if stop did not stop it first.
func (n *clusterNode) start(t *testing.T) {
	t.Helper()
	if err := n.tryStart(t); err != nil {
		t.Fatal(err)
	}
}

// tryStart is start that returns what fails New, instead of failing the test.
func (n *clusterNode) tryStart(t *testing.T) error {
	t.Helper()
	begin := time.Now()
	a, err := New(context.Background(), n.cfg, zerolog.New(n.logs), WithPasswordCost(bcrypt.MinCost))
	if err != nil {
		return fmt.Errorf("start %s: %w", n.id, err)
	}
	took := time.Since(begin)
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	n.runDone = make(chan struct{})
	go func() {
		err := a.Run(ctx)
		n.runErr = err
		close(n.runDone)
		ran <- err
	}()
	n.app = a
	n.stop = sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-ran:
			if !errors.Is(err, context.Canceled) && !n.discarded {
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
		return fmt.Errorf("New took %s to start %s: it must not wait for a leader", took, n.id)
	}
	return nil
}

// logLine is a line that a node logged: its index among the node's lines, and
// its fields.
type logLine struct {
	index  int
	fields map[string]any
}

// logged returns, in order, the lines n logged that hold every field of want.
func (n *clusterNode) logged(t *testing.T, want map[string]any) []logLine {
	t.Helper()
	var out []logLine
	for i, line := range strings.Split(strings.TrimSpace(n.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("%s logged %q: %v", n.id, line, err)
		}
		matches := true
		for key, value := range want {
			matches = matches && fields[key] == value
		}
		if matches {
			out = append(out, logLine{index: i, fields: fields})
		}
	}
	return out
}

// failedSeeds returns the lines in which n's bootstrap runner reported a seed
// that failed after it wrote n's password file: the WARN of a run it retries,
// or the INFO of a run that the end of its term ended, whose error names the
// file.
func (n *clusterNode) failedSeeds(t *testing.T) []logLine {
	t.Helper()
	var out []logLine
	for _, line := range n.logged(t, map[string]any{"runner": "bootstrap"}) {
		text, _ := line.fields["error"].(string)
		rest, wrote := strings.CutPrefix(text, storeFailed)
		if message := line.fields["message"]; !wrote || (message != termTaskRetry && message != termTaskEnded) {
			continue
		}
		if path, _, _ := strings.Cut(rest, ": "); path != n.passwordFilePath() {
			t.Fatalf("%s reported a seed that wrote %s, not its own password file", n.id, path)
		}
		out = append(out, line)
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

// seedOutcome is a seed read as the bootstrap README tells an operator to read
// it: the node whose password file to trust, the line of its log that names
// that file, and the administrator's stored password hash, the same on every
// node.
type seedOutcome struct {
	node *clusterNode
	line logLine
	hash string
}

// waitForSeed waits until every node holds the same administrator and some
// bootstrap run finished after the seed, then reads the seed as the bootstrap
// README tells an operator to. The file to trust is the one of the node that
// logged "admin password generated"; when no node did, it is the file named by
// a seed that failed after writing it, and whose write took effect. A
// leadership change during the seed leaves either outcome: one generated
// password and a stale file on the leader whose seed failed, or no generated
// password and a valid file on the leader whose store returned a leadership
// error. waitForSeed fails the test unless every node holds exactly one
// administrator with the same stored password, the trusted file holds that
// password, no more than one password was generated, and every other password
// file belongs to a node whose seed failed after writing it. The node whose
// file it trusts logged the start of its bootstrap runner's term before the
// line that names the file.
func waitForSeed(t *testing.T, nodes []*clusterNode) seedOutcome {
	t.Helper()
	var hash string
	waitFor(t, 20*time.Second, "an administrator that every node holds alike, and a bootstrap run done after its seed", func() bool {
		for i, n := range nodes {
			got, err := seededAdmin(n.app.store)
			if err != nil || (i > 0 && got != hash) {
				return false
			}
			hash = got
		}
		for _, n := range nodes {
			if len(n.logged(t, map[string]any{"runner": "bootstrap", "message": termTaskDone})) > 0 {
				return true
			}
		}
		return false
	})

	seed := seedOutcome{hash: hash}
	generated, total := map[string]int{}, 0 // the generated lines of each node that logged any, and their sum
	for _, n := range nodes {
		lines := n.logged(t, map[string]any{"message": passwordGenerated})
		if len(lines) > 0 {
			generated[n.id] = len(lines)
			total += len(lines)
		}
		if len(lines) == 1 && seed.node == nil {
			seed.node, seed.line = n, lines[0]
		}
	}
	if total > 1 {
		t.Fatalf("a password was generated %d times, want once at most: %v", total, generated)
	}
	if seed.node != nil {
		if path := seed.line.fields["path"]; path != seed.node.passwordFilePath() {
			t.Fatalf("%s logged its generated password in %v, want %s", seed.node.id, path, seed.node.passwordFilePath())
		}
		if password, ok := seed.node.passwordFile(t); !ok || !verifiesAdmin(t, seed.node.app.store, password) {
			t.Fatalf("the password file of %s, which generated the password, does not hold the stored one", seed.node.id)
		}
	} else {
		// No node generated a password: a seed that failed after writing its
		// file took effect, and its file holds the stored password.
		for _, n := range nodes {
			failed := n.failedSeeds(t)
			if len(failed) == 0 {
				continue
			}
			if password, ok := n.passwordFile(t); ok && verifiesAdmin(t, n.app.store, password) {
				seed.node, seed.line = n, failed[len(failed)-1]
				break
			}
		}
		if seed.node == nil {
			t.Fatal("no node generated a password, and no file that a failed seed wrote holds the stored one")
		}
	}
	if id := seed.line.fields["node_id"]; id != seed.node.id {
		t.Fatalf("the line that names the password file of %s names node %v", seed.node.id, id)
	}
	acquired := seed.node.logged(t, map[string]any{"runner": "bootstrap", "message": acquiredLeadership})
	if len(acquired) == 0 || acquired[0].index > seed.line.index {
		t.Fatalf("%s seeded before its bootstrap runner acquired leadership", seed.node.id)
	}
	for _, n := range nodes {
		if _, ok := n.passwordFile(t); ok && n != seed.node && len(n.failedSeeds(t)) == 0 {
			t.Fatalf("%s has a password file, though it generated no password and failed no seed after writing one", n.id)
		}
	}
	return seed
}

// A node that starts alone has no quorum: New starts it without waiting for a
// leader, and nothing is seeded while no node can lead. The other two nodes
// start several election timeouts later, so no window that opens at a node's
// start could see the quorum form, and the leader of the quorum seeds at its
// leadership gain: exactly one administrator, the same on every node, whose
// password lies in the password file the bootstrap README says to trust, which
// the line that names it ties to its node.
func TestLateQuorumStillSeedsOnce(t *testing.T) {
	nodes := clusterNodes(t, "n1", "n2", "n3")
	first := nodes[0]
	first.start(t)

	// A fixed delay: n1's raft term is out of this package's reach, and pre-vote keeps it still.
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
	waitForSeed(t, nodes)
}

// Once the administrator is seeded, another node runs bootstrap and changes
// nothing: one administrator, the same stored password, no password generated,
// and no password file on another node but one whose seed failed after writing
// it. The file to trust stays in its data directory.
func TestSeedingIsIdempotentAcrossLeaders(t *testing.T) {
	nodes := clusterNodes(t, "n1", "n2", "n3")
	for _, n := range nodes {
		n.start(t)
	}
	seed := waitForSeed(t, nodes)
	password, ok := seed.node.passwordFile(t)
	if !ok {
		t.Fatalf("the password file of %s went away", seed.node.id)
	}
	var others []*clusterNode
	for _, n := range nodes {
		if n != seed.node {
			others = append(others, n)
		}
	}

	// The node whose file holds the password stops, and leadership leaves it if
	// it still leads. A bootstrap run that another node finished after that
	// logged "done", which a run that seeded logs too, after its "generated"
	// line: so the checks below make the test, not the wait. Every node held
	// the administrator before the stop (waitForSeed), so a run that seeded again
	// would leave a "generated" line on another node, or another stored hash.
	seed.node.stop()
	done := map[string]any{"runner": "bootstrap", "message": termTaskDone}
	var next *clusterNode
	waitFor(t, 15*time.Second, "bootstrap done on another node", func() bool {
		for _, n := range others {
			if len(n.logged(t, done)) > 0 {
				next = n
				return true
			}
		}
		return false
	})
	for _, n := range others {
		got, err := seededAdmin(n.app.store)
		if err != nil {
			t.Fatalf("%s after bootstrap ran on %s: %v", n.id, next.id, err)
		}
		if got != seed.hash {
			t.Fatalf("%s holds another password after bootstrap ran on %s: the seed ran again", n.id, next.id)
		}
		if _, ok := n.passwordFile(t); ok && len(n.failedSeeds(t)) == 0 {
			t.Fatalf("%s has a password file, though it failed no seed after writing one", n.id)
		}
		if lines := n.logged(t, map[string]any{"message": passwordGenerated}); len(lines) != 0 {
			t.Fatalf("%s generated a password", n.id)
		}
	}
	if got, ok := seed.node.passwordFile(t); !ok || got != password {
		t.Fatalf("the password file of %s changed or went away", seed.node.id)
	}
}

// A replica that takes leadership counts an agent's silence from the start of
// its own term, not from its process start. Every node runs the lifecycle
// runner, built by lifecycle.New as New builds it, from the same start. The
// agent last synced an hour ago, and delete_after is short. The first leader
// counts the silence from its own term, and stops halfway to deleting the
// agent. The next term starts after that stop, so a leader that counts from
// its term keeps the agent for a full delete_after after it, where one that
// counted from its process start would delete the agent about delete_after/2
// after it. The next leader then deletes the agent.
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
		pool := proxy.NewPool(nil, false)
		t.Cleanup(func() { _ = pool.Close() })
		r, err := lifecycle.New(cfg, zerolog.Nop(), n.store, pool, n.hub, n.leadership)
		if err != nil {
			t.Fatal(err)
		}
		runners[n] = r
		go func() { _ = r.Start(context.Background()) }()
		t.Cleanup(func() { _ = r.Stop(context.Background()) })
	}
	started := time.Now()

	// A deliberate delay, not a wait for a condition: the first leader leads a
	// while, so that the runners' start lies well before the next term.
	time.Sleep(deleteAfter / 2)
	if !stored(leader) {
		t.Fatalf("%s deleted the agent %s after the runners started, before delete_after", leader.id, time.Since(started))
	}
	// The leader stops as its process would: its runners first, then raft. The
	// next term starts after stopAt.
	stopAt := time.Now()
	if err = runners[leader].Stop(ctx); err != nil {
		t.Fatal(err)
	}
	leader.stop()
	next, _ := readyLeader(t, followers)

	// Only a read that missed the agent, and ended before delete_after passed
	// since stopAt, fails the test: the time is taken after each read.
	for {
		present := stored(next)
		if time.Since(stopAt) >= deleteAfter {
			break
		}
		if !present {
			t.Fatalf("%s deleted the agent %s after the first leader stopped, %s after its runner started: it counted silence from before its term",
				next.id, time.Since(stopAt), time.Since(started))
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, 5*time.Second, next.id+" deletes the agent delete_after into its term", func() bool { return !stored(next) })
}
