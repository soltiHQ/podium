package raft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// checkLine is a line of a configuration check: its level, message, leader
// and the groups it names.
type checkLine struct {
	level, message, leader string
	groups                 []string
}

func (l checkLine) String() string {
	return fmt.Sprintf("%s %q leader=%s groups=%v", l.level, l.message, l.leader, l.groups)
}

// checkLines returns the lines a configuration check logged into logs, but the
// one of its start.
func checkLines(t *testing.T, logs *logBuffer) []checkLine {
	t.Helper()
	var out []checkLine
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if raw == "" {
			continue
		}
		var fields struct {
			Level   string   `json:"level"`
			Message string   `json:"message"`
			Runner  string   `json:"runner"`
			Leader  string   `json:"leader_id"`
			Groups  []string `json:"groups"`
		}
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			t.Fatalf("the check logged %q: %v", raw, err)
		}
		if fields.Runner != configCheckName {
			t.Fatalf("the check logged %q without its runner", raw)
		}
		if fields.Message == "runner started" {
			continue
		}
		out = append(out, checkLine{level: fields.Level, message: fields.Message, leader: fields.Leader, groups: fields.Groups})
	}
	return out
}

// stubSource is what the configuration check of a stub test reads: the leader
// the node knows, what a fetch of each address gives, and the wake it watches.
type stubSource struct {
	mu       sync.Mutex
	id, addr string
	answers  map[string]cluster.Digests // what a fetch of an address answers
	failures map[string]int             // how many fetches of an address fail first; -1 for every one
	wake     chan<- struct{}

	asked   chan string // the ID of the leader each read of the leader gives
	fetched chan string // the address of each fetch
	watched chan struct{}
}

func newStubSource() *stubSource {
	return &stubSource{
		answers:  map[string]cluster.Digests{},
		failures: map[string]int{},
		asked:    make(chan string, 64),
		fetched:  make(chan string, 64),
		watched:  make(chan struct{}),
	}
}

func (s *stubSource) leader() (string, string) {
	s.mu.Lock()
	id, addr := s.id, s.addr
	s.mu.Unlock()
	s.asked <- id
	return id, addr
}

func (s *stubSource) fetch(_ context.Context, addr string) (cluster.Digests, error) {
	s.mu.Lock()
	answer, failures := s.answers[addr], s.failures[addr]
	if failures > 0 {
		s.failures[addr]--
	}
	s.mu.Unlock()
	s.fetched <- addr
	if failures != 0 {
		return cluster.Digests{}, errors.New("dial tcp " + addr + ": connection refused")
	}
	return answer, nil
}

func (s *stubSource) watchLeader(wake chan<- struct{}) func() {
	s.wake = wake
	close(s.watched)
	return func() {}
}

// lead makes the node know id, at addr, as its leader, and wakes its check.
func (s *stubSource) lead(id, addr string) {
	s.mu.Lock()
	s.id, s.addr = id, addr
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// receive returns the next value of ch, and fails the test after 5s without one.
func receive(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s within 5s", what)
		return ""
	}
}

// runStubCheck runs the configuration check of node n1, whose digests are own,
// over src, until the test ends, and returns it with the buffer it logs into.
// after replaces the timer of its retry pauses.
func runStubCheck(t *testing.T, src *stubSource, own cluster.Digests, after func(time.Duration) <-chan time.Time) (*ConfigCheck, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	check := newConfigCheck("n1", own, src.leader, src.fetch, src.watchLeader, zerolog.New(logs))
	check.after = after
	done := make(chan error, 1)
	go func() { done <- check.Start(context.Background()) }()
	t.Cleanup(func() {
		if err := check.Stop(context.Background()); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Errorf("Start() = %v, want nil once stopped", err)
		}
	})
	select {
	case <-src.watched:
	case <-time.After(5 * time.Second):
		t.Fatal("the check does not watch the leader within 5s")
	}
	return check, logs
}

// stopCheck stops check and returns its lines: every line it logs comes before
// Stop returns.
func stopCheck(t *testing.T, check *ConfigCheck, logs *logBuffer) []checkLine {
	t.Helper()
	if err := check.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	return checkLines(t, logs)
}

// immediately is a timer of retry pauses that records each pause and fires at
// once.
type immediately struct {
	mu     sync.Mutex
	pauses []time.Duration
}

func (i *immediately) after(d time.Duration) <-chan time.Time {
	i.mu.Lock()
	i.pauses = append(i.pauses, d)
	i.mu.Unlock()
	fired := make(chan time.Time)
	close(fired)
	return fired
}

// never is a timer of retry pauses that never fires: only a change of leader,
// or the end of the check, ends a pause.
func never(time.Duration) <-chan time.Time { return nil }

// A check compares the node's digests with its leader's, at its start and at
// each change of the leader the node knows, and logs a mismatch at ERROR once
// per leader and digest set, naming the groups that differ: a leader seen
// again with the digests it was logged with gives a DEBUG line only, and so
// does a leader whose digests match, while a match after a mismatch is one INFO
// line, after which a mismatch is logged again. A node that leads, or knows no
// leader, checks nothing.
func TestConfigCheckLogsEachMismatchOnce(t *testing.T) {
	own := testDigests(1)
	proxies, agentAuth := own, own
	proxies[2][0] ^= 0xFF
	agentAuth[1][9] ^= 0xFF
	src := newStubSource()
	src.id, src.addr = "n2", "10.0.0.2:7000"
	src.answers["10.0.0.2:7000"] = proxies
	src.answers["10.0.0.3:7000"] = proxies
	check, logs := runStubCheck(t, src, own, never)

	// step changes what a leader answers, makes the node know the leader id,
	// and waits until the check read it and, unless fetch is false, fetched it.
	step := func(id, addr string, answers *cluster.Digests, fetch bool) {
		t.Helper()
		src.mu.Lock()
		if answers != nil {
			src.answers[addr] = *answers
		}
		src.mu.Unlock()
		src.lead(id, addr)
		if got := receive(t, src.asked, "read of the leader "+id); got != id {
			t.Fatalf("the check read the leader %q, want %q", got, id)
		}
		if !fetch {
			return
		}
		if got := receive(t, src.fetched, "fetch from "+addr); got != addr {
			t.Fatalf("the check fetched %s, want %s", got, addr)
		}
	}
	// At its start, the check reads the leader n2 and fetches from it.
	receive(t, src.asked, "read of the leader at the start")
	receive(t, src.fetched, "fetch at the start")
	step("n2", "10.0.0.2:7000", nil, true)        // n2 again, as after a flap through no leader
	step("n3", "10.0.0.3:7000", nil, true)        // another leader, with the same digests
	step("n2", "10.0.0.2:7000", nil, true)        // n2 again, still with its digests
	step("n2", "10.0.0.2:7000", &agentAuth, true) // n2 with other digests
	step("n3", "10.0.0.3:7000", &own, true)       // n3 now shares this node's settings
	step("n3", "10.0.0.3:7000", nil, true)        // and still does
	step("n1", "10.0.0.1:7000", nil, false)       // this node leads
	step("", "", nil, false)                      // no leader known
	step("n2", "10.0.0.2:7000", nil, true)        // n2, with the digests logged before the match
	if len(src.fetched) != 0 {
		t.Fatalf("the check fetched %d more times than the steps asked", len(src.fetched))
	}

	differs := func(level, leader string, groups ...string) checkLine {
		return checkLine{level: level, message: configDiffers, leader: leader, groups: groups}
	}
	matches := func(level, leader string) checkLine {
		return checkLine{level: level, message: configMatches, leader: leader}
	}
	want := []checkLine{
		differs("error", "n2", cluster.GroupProxies),
		differs("debug", "n2", cluster.GroupProxies),
		differs("error", "n3", cluster.GroupProxies),
		differs("debug", "n2", cluster.GroupProxies),
		differs("error", "n2", cluster.GroupAgentAuth),
		matches("info", "n3"),
		matches("debug", "n3"),
		differs("error", "n2", cluster.GroupAgentAuth),
	}
	if got := stopCheck(t, check, logs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the check logged\n%v\nwant\n%v", got, want)
	}
}

// A check whose fetch fails logs one WARN and retries, after a pause of a
// second that doubles after each failure up to a minute, until a fetch
// succeeds: a match then logs one INFO, as it ends the warning. A change of
// leader during a pause ends it, and the check fetches from the new leader at
// once; a node that comes to lead retries nothing.
func TestConfigCheckRetriesAFailedFetch(t *testing.T) {
	own := testDigests(1)
	drifted := own
	drifted[3][0] ^= 0xFF
	warn := func(leader string) checkLine {
		return checkLine{level: "warn", message: configFetchFailed, leader: leader}
	}

	t.Run("until a fetch succeeds", func(t *testing.T) {
		src := newStubSource()
		src.id, src.addr = "n2", "10.0.0.2:7000"
		src.answers["10.0.0.2:7000"] = own
		src.failures["10.0.0.2:7000"] = 8
		timer := &immediately{}
		check, logs := runStubCheck(t, src, own, timer.after)
		for range 9 {
			receive(t, src.fetched, "fetch from n2")
		}
		got := stopCheck(t, check, logs)
		want := []checkLine{warn("n2"), {level: "info", message: configMatches, leader: "n2"}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("the check logged\n%v\nwant\n%v", got, want)
		}
		wantPauses := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
		if !slices.Equal(timer.pauses, wantPauses) {
			t.Fatalf("the check paused %v between its fetches, want %v", timer.pauses, wantPauses)
		}
	})

	t.Run("until the leader changes", func(t *testing.T) {
		src := newStubSource()
		src.id, src.addr = "n2", "10.0.0.2:7000"
		src.failures["10.0.0.2:7000"] = -1
		src.answers["10.0.0.3:7000"] = drifted
		check, logs := runStubCheck(t, src, own, never)
		receive(t, src.asked, "read of the leader at the start")
		receive(t, src.fetched, "fetch from n2")
		src.lead("n3", "10.0.0.3:7000")
		receive(t, src.asked, "read of the leader n3")
		if got := receive(t, src.fetched, "fetch from n3"); got != "10.0.0.3:7000" {
			t.Fatalf("the check fetched %s, want n3's address", got)
		}
		src.lead("n1", "10.0.0.1:7000")
		receive(t, src.asked, "read of the leader n1")
		got := stopCheck(t, check, logs)
		want := []checkLine{warn("n2"), {level: "error", message: configDiffers, leader: "n3", groups: []string{cluster.GroupCORS}}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("the check logged\n%v\nwant\n%v", got, want)
		}
		if len(src.fetched) != 0 {
			t.Fatal("the check fetched while this node led")
		}
	})

	t.Run("until this node leads", func(t *testing.T) {
		src := newStubSource()
		src.id, src.addr = "n2", "10.0.0.2:7000"
		src.failures["10.0.0.2:7000"] = -1
		check, logs := runStubCheck(t, src, own, never)
		receive(t, src.asked, "read of the leader at the start")
		receive(t, src.fetched, "fetch from n2")
		src.lead("n1", "10.0.0.1:7000")
		receive(t, src.asked, "read of the leader n1")
		got := stopCheck(t, check, logs)
		if want := []checkLine{warn("n2")}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("the check logged\n%v\nwant\n%v", got, want)
		}
		if len(src.fetched) != 0 {
			t.Fatal("the check fetched again once this node led")
		}
	})
}

// moveLeadership makes the node id of c lead, by a transfer of hashicorp/raft
// to it, and waits until every node knows it as the leader.
func moveLeadership(t *testing.T, c *loopbackCluster, id string) {
	t.Helper()
	leader := c.leader(t, 5*time.Second)
	if leader.id != id {
		var target *loopbackNode
		for _, n := range c.nodes {
			if n.id == id {
				target = n
			}
		}
		if err := leader.profile.raft.LeadershipTransferToServer(hraft.ServerID(id), hraft.ServerAddress(target.addr)).Error(); err != nil {
			t.Fatalf("transfer the leadership of %s to %s: %v", leader.id, id, err)
		}
	}
	waitFor(t, 5*time.Second, "every node knows "+id+" as its leader", func() bool {
		for _, n := range c.nodes {
			if leaderID, _ := n.profile.Admin().Leader(); leaderID != id {
				return false
			}
		}
		return true
	})
}

// waitForLine waits until the check that logs into logs logged want.
func waitForLine(t *testing.T, node string, logs *logBuffer, want checkLine) {
	t.Helper()
	waitFor(t, 10*time.Second, node+" logs "+want.String(), func() bool {
		for _, line := range checkLines(t, logs) {
			if line.String() == want.String() {
				return true
			}
		}
		return false
	})
}

// On real raft nodes over mutual TLS, the check of every follower fetches its
// leader's digests over the raft port, at its start and again at each change
// of leader, which leadership transfers make here: n1 and n2 share their
// settings, and n3's differ in auth and CORS. While n1 and then n2 lead, n3
// logs one ERROR for each, naming auth and CORS, and the other follower logs a
// match at DEBUG. When n3 leads, it checks nothing, and n1 and n2 each log the
// ERROR: a drifted node that comes to lead is caught too. When n1 leads again,
// n2's match ends its mismatch with one INFO, and n3, which logged n1's digests
// already, logs them at DEBUG only. No other ERROR or INFO line is logged.
func TestConfigCheckFollowsTheLeader(t *testing.T) {
	shared, drifted := testDigests(1), testDigests(1)
	drifted[0][0] ^= 0xFF
	drifted[3][0] ^= 0xFF
	c := loopbackNodes(t, "n1", "n2", "n3")
	digests := map[string]cluster.Digests{"n1": shared, "n2": shared, "n3": drifted}
	for _, n := range c.nodes {
		n.start(t, c.discovery(t), withConfigDigests(digests[n.id]))
	}
	moveLeadership(t, c, "n1")

	logs := map[string]*logBuffer{}
	for _, n := range c.nodes {
		logs[n.id] = &logBuffer{}
		check := n.profile.Admin().ConfigCheck(zerolog.New(logs[n.id]))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = check.Start(ctx)
		}()
		t.Cleanup(func() {
			cancel()
			<-done
		})
	}
	differs := func(level, leader string) checkLine {
		return checkLine{level: level, message: configDiffers, leader: leader, groups: []string{cluster.GroupAuth, cluster.GroupCORS}}
	}
	matches := func(level, leader string) checkLine {
		return checkLine{level: level, message: configMatches, leader: leader}
	}

	waitForLine(t, "n3", logs["n3"], differs("error", "n1"))
	waitForLine(t, "n2", logs["n2"], matches("debug", "n1"))

	moveLeadership(t, c, "n2")
	waitForLine(t, "n3", logs["n3"], differs("error", "n2"))
	waitForLine(t, "n1", logs["n1"], matches("debug", "n2"))

	moveLeadership(t, c, "n3")
	waitForLine(t, "n1", logs["n1"], differs("error", "n3"))
	waitForLine(t, "n2", logs["n2"], differs("error", "n3"))

	moveLeadership(t, c, "n1")
	waitForLine(t, "n2", logs["n2"], matches("info", "n1"))
	waitForLine(t, "n3", logs["n3"], differs("debug", "n1"))

	want := map[string][]checkLine{
		"n1": {differs("error", "n3")},
		"n2": {differs("error", "n3"), matches("info", "n1")},
		"n3": {differs("error", "n1"), differs("error", "n2")},
	}
	for _, n := range c.nodes {
		var got []checkLine
		for _, line := range checkLines(t, logs[n.id]) {
			if line.level != "debug" {
				got = append(got, line)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want[n.id]) {
			t.Fatalf("%s's check logged\n%v\nabove DEBUG, want\n%v", n.id, got, want[n.id])
		}
	}
}
