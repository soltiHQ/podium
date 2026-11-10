package app

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
	"golang.org/x/crypto/bcrypt"
)

const (
	// configCheck is the name of the configuration check's runner.
	configCheck = "config-check"
	// What the configuration check logs for a leader whose digests differ from
	// this node's, and for one whose digests match.
	configDiffers = "configuration check: this node's configuration differs from the leader's in settings that every replica must share"
	configMatches = "configuration check: this node's configuration matches the leader's"
)

// leaderOf waits until one of nodes leads and every other knows it, and
// returns it with the others.
func leaderOf(t *testing.T, nodes []*clusterNode) (leader *clusterNode, others []*clusterNode) {
	t.Helper()
	waitFor(t, 10*time.Second, "one leader that every running node knows", func() bool {
		leader, others = nil, nil
		for _, n := range nodes {
			if n.app.leadership.AmLeader() {
				if leader != nil {
					return false
				}
				leader = n
				continue
			}
			others = append(others, n)
		}
		if leader == nil {
			return false
		}
		for _, n := range others {
			if n.app.leadership.CurrentLeader() != leader.cfg.Cluster.Raft.BindAddr {
				return false
			}
		}
		return true
	})
	return leader, others
}

// secretForms returns what no log line may hold of cfg's settings that
// replicas must share: its secrets, its CORS origins, and its fingerprint and
// digests, as hex and as base64.
func secretForms(t *testing.T, cfg config.Config) []string {
	t.Helper()
	digests := digestsOf(t, cfg)
	forms := []string{cfg.Auth.JWTSecret, digests.Fingerprint()}
	forms = append(forms, cfg.CORS.AllowOrigins...)
	for _, g := range digests {
		forms = append(forms, hex.EncodeToString(g[:]), base64.StdEncoding.EncodeToString(g[:]))
	}
	return forms
}

// Three whole raft nodes run as main runs them, over mutual TLS: n1 and n2
// share their settings, and n3's jwt_secret and CORS origins differ. n1 and n2
// elect a leader before n3 starts, so n3 follows it. The configuration check of
// n3 then logs one ERROR for each leader it follows, which names that leader
// and the groups auth and CORS, while the check of the other follower finds the
// leader's digests match. The checks of n1 and n2 log an ERROR, as one, only
// for n3, should n3 come to lead. No line of any node holds a secret, a CORS
// origin, a digest or a fingerprint. The status of each node gives the
// fingerprint of its own settings: n1's and n2's alike, n3's another.
func TestMismatchedReplicaLogsError(t *testing.T) {
	nodes := clusterNodes(t, "n1", "n2", "n3")
	for _, n := range nodes {
		n.cfg.CORS.AllowOrigins = []string{"https://console.example"}
	}
	drifted := nodes[2]
	drifted.cfg.Auth.JWTSecret = strings.Repeat("drifted-replica-signing-secret", 2)
	drifted.cfg.CORS.AllowOrigins = []string{"https://drifted-console.example"}

	for _, n := range nodes[:2] {
		n.start(t)
	}
	leader, matching := leaderOf(t, nodes[:2])
	drifted.start(t)

	checkErrors := map[string]any{"runner": configCheck, "level": "error"}
	waitFor(t, 15*time.Second, drifted.id+" logs the ERROR of its configuration check", func() bool {
		return len(drifted.logged(t, checkErrors)) > 0
	})
	waitFor(t, 15*time.Second, matching[0].id+" finds its leader's configuration matches", func() bool {
		return len(matching[0].logged(t, map[string]any{"runner": configCheck, "message": configMatches, "leader_id": leader.id})) > 0
	})

	// errorsByLeader counts the ERROR lines of n's check by the leader each
	// names, and fails the test unless each says the configuration differs in
	// auth and CORS.
	errorsByLeader := func(n *clusterNode) map[string]int {
		counts := map[string]int{}
		for _, line := range n.logged(t, checkErrors) {
			var groups []string
			if raw, ok := line.fields["groups"].([]any); ok {
				for _, g := range raw {
					groups = append(groups, fmt.Sprint(g))
				}
			}
			if line.fields["message"] != configDiffers || !slices.Equal(groups, []string{cluster.GroupAuth, cluster.GroupCORS}) {
				t.Fatalf("%s logged %v, want %q naming the groups [auth CORS]", n.id, line.fields, configDiffers)
			}
			counts[fmt.Sprint(line.fields["leader_id"])]++
		}
		return counts
	}
	// The drifted node's ERRORs name the leaders it followed: n1 or n2, which
	// share their settings, never itself, nor another ID or none.
	followed := map[string]bool{nodes[0].id: true, nodes[1].id: true}
	for id, count := range errorsByLeader(drifted) {
		if !followed[id] {
			t.Fatalf("%s logged an ERROR for the leader %q, want %s or %s", drifted.id, id, nodes[0].id, nodes[1].id)
		}
		if count != 1 {
			t.Fatalf("%s logged %d ERROR lines for the leader %s, want one", drifted.id, count, id)
		}
	}
	for _, n := range nodes[:2] {
		for id, count := range errorsByLeader(n) {
			if id != drifted.id || count != 1 {
				t.Fatalf("%s logged %d ERROR lines for the leader %s, want one, and only while %s leads", n.id, count, id, drifted.id)
			}
		}
	}

	var forbidden []string
	for _, n := range nodes {
		forbidden = append(forbidden, secretForms(t, n.cfg)...)
	}
	for _, n := range nodes {
		out := n.logs.String()
		for _, form := range forbidden {
			if strings.Contains(out, form) {
				t.Fatalf("%s logged %q, a value or a digest of the settings replicas must share", n.id, form)
			}
		}
	}

	ctx := context.Background()
	fingerprints := map[string]string{}
	for _, n := range nodes {
		got := n.app.cluster.Status(ctx).Fingerprint
		if want := digestsOf(t, n.cfg).Fingerprint(); got != want {
			t.Fatalf("%s's status gives the fingerprint %q, want %q, its own settings'", n.id, got, want)
		}
		fingerprints[n.id] = got
	}
	if fingerprints["n1"] != fingerprints["n2"] || fingerprints["n1"] == fingerprints["n3"] {
		t.Fatalf("fingerprints %v, want n1's and n2's alike and n3's another", fingerprints)
	}
}

// Standalone digests nothing and checks nothing: New gives the app no cluster
// service, whose status would give a fingerprint, and Run starts the seven
// runners of standalone, the configuration check not among them.
func TestStandaloneRunsNoConfigCheck(t *testing.T) {
	logs := &syncBuffer{}
	cfg := config.Default()
	cfg.Cluster.Standalone.Ephemeral = true
	cfg.Auth.JWTSecret = strings.Repeat("standalone-config-check-secret", 2)
	cfg.HTTP.Addr, cfg.HTTPDiscovery.Addr, cfg.GRPC.Addr = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
	cfg.Ops.Addr = "" // the ops listener is an eighth runner, which this test does not count
	a, err := New(context.Background(), cfg, zerolog.New(logs), WithPasswordCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	if a.cluster != nil {
		t.Fatal("a standalone app has a cluster service")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-ran; !errors.Is(err, context.Canceled) {
			t.Errorf("Run ended with %v, want %v once canceled", err, context.Canceled)
		}
		if err := a.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	node := &clusterNode{id: "standalone", logs: logs}
	starting := map[string]any{"message": "runner starting"}
	waitFor(t, 10*time.Second, "the runners of standalone start", func() bool { return len(node.logged(t, starting)) >= 7 })
	// A deliberate delay, not a wait for a condition: the server starts every
	// runner at once, so an eighth would log its start within it.
	time.Sleep(100 * time.Millisecond)
	var names []string
	for _, line := range node.logged(t, starting) {
		names = append(names, fmt.Sprint(line.fields["runner"]))
	}
	if len(names) != 7 || slices.Contains(names, configCheck) {
		t.Fatalf("standalone started the runners %v, want seven without %s", names, configCheck)
	}
}
