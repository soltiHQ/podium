package app

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// The failover of the database mode under load: three apps on one database,
// each through a TCP proxy of its own, agents syncing through all three, and
// the leader cut off the database, as a partition cuts it.

const (
	// failoverLease is the lease of the failover's replicas. A follower reads the
	// lease row every second (the leadership's poll interval, which the app's
	// configuration does not set): the renewal interval, a third of the lease,
	// 1.5 s, exceeds that read with a margin.
	failoverLease = 4500 * time.Millisecond
	// leaderOnlyRunners is how many runners of an app of the database mode run
	// their work under WhenLeader, agent auth required as by default: lifecycle,
	// sync, history, agent-credentials and bootstrap.
	leaderOnlyRunners = 5
	// failoverAgents agents sync through the apps, each every failoverSyncEvery:
	// within an agent's sync budget on its leader, ten at once, then one a
	// second, and often enough together that a sync reaches a replica soon after
	// the takeover.
	failoverAgents    = 9
	failoverSyncEvery = 400 * time.Millisecond
	// forwardedSyncs is the series of the discovery syncs a follower forwarded.
	forwardedSyncs = `podium_forwarded_writes_total{server="discovery"}`
	// workEndTolerance is how much later than the step-down's bound the
	// leader-only work of the cut-off leader may end: the step-down cancels the
	// work's contexts at its deadline, and the runners then return once they
	// are scheduled, a few milliseconds later on a busy machine under -race.
	workEndTolerance = 100 * time.Millisecond
)

// withLeaderWork makes New run the work of each leader-only runner through
// watch, a test hook that production never sets.
func withLeaderWork(watch func(work func(context.Context) error) func(context.Context) error) Option {
	return func(o *options) { o.leaderWork = watch }
}

// leaderWork follows the work of an app's leader-only runners, through the
// hook withLeaderWork installs: each call of fn that a runner runs under
// WhenLeader, once per term, which returns once the term ends.
type leaderWork struct {
	mu sync.Mutex
	workState
}

// workState is what leaderWork saw: the calls that run, the calls started in
// all, when the first started, and when the count of those that run last fell
// to zero.
type workState struct {
	running, started  int
	firstStart, ended time.Time
}

// watch is the hook: fn, counted while it runs.
func (w *leaderWork) watch(fn func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		w.mu.Lock()
		if w.started == 0 {
			w.firstStart = time.Now()
		}
		w.running++
		w.started++
		w.mu.Unlock()
		defer func() {
			w.mu.Lock()
			w.running--
			if w.running == 0 {
				w.ended = time.Now()
			}
			w.mu.Unlock()
		}()
		return fn(ctx)
	}
}

// state returns what w saw so far.
func (w *leaderWork) state() workState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.workState
}

// dbProxy is a TCP proxy between one app and the Postgres server of the tests.
// The app's database URL names the proxy, so that every pool of the app and its
// events listener connect through it, and cut partitions that app alone from
// the database.
type dbProxy struct {
	listener net.Listener
	server   string // the Postgres server's host:port
	done     sync.WaitGroup

	mu sync.Mutex
	// conns holds both ends of every connection the proxy carries.
	conns map[net.Conn]struct{}
	// isCut: the proxy carries nothing more.
	isCut bool
}

// newDBProxy starts a proxy on the loopback to the server of databaseURL, and
// returns it with databaseURL pointed at it. It stops when t ends.
func newDBProxy(t *testing.T, databaseURL string) (*dbProxy, string) {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal("the database URL does not parse")
	}
	server := u.Host
	if u.Port() == "" {
		server = net.JoinHostPort(u.Hostname(), "5432")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &dbProxy{listener: l, server: server, conns: map[net.Conn]struct{}{}}
	p.done.Add(1)
	go p.serve()
	t.Cleanup(p.close)
	u.Host = l.Addr().String()
	return p, u.String()
}

// serve accepts connections until the listener closes, and carries each.
func (p *dbProxy) serve() {
	defer p.done.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.done.Add(1)
		go p.carry(client)
	}
}

// carry carries client to a connection of its own to the server, both ways,
// until either end closes, or the proxy is cut; once it is, it closes client at
// once.
func (p *dbProxy) carry(client net.Conn) {
	defer p.done.Done()
	if p.cutOff() {
		_ = client.Close()
		return
	}
	server, err := net.DialTimeout("tcp", p.server, 5*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	p.mu.Lock()
	if p.isCut {
		p.mu.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	p.conns[client], p.conns[server] = struct{}{}, struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.conns, client)
		delete(p.conns, server)
		p.mu.Unlock()
	}()
	copied := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(server, client); copied <- struct{}{} }()
	go func() { _, _ = io.Copy(client, server); copied <- struct{}{} }()
	<-copied
	_ = client.Close()
	_ = server.Close()
	<-copied
}

// cut closes every connection the proxy carries, both ends, so that the server
// ends their sessions at once, and refuses every new one: the app is cut off
// the database, as a partition cuts it, while the others reach it.
func (p *dbProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.isCut = true
	for c := range p.conns {
		_ = c.Close()
	}
}

// cutOff reports whether the proxy is cut.
func (p *dbProxy) cutOff() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isCut
}

// close stops the proxy and waits for its goroutines.
func (p *dbProxy) close() {
	_ = p.listener.Close()
	p.cut()
	p.done.Wait()
}

// syncAttempt is one sync of a simulated agent: the app it was sent to, the
// sequence it carried, when it was sent and answered, and the status of the
// answer, 0 when none came.
type syncAttempt struct {
	via            *clusterNode
	seq            int64
	sent, answered time.Time
	status         int
}

// agentLoad is the simulated agents of TestDatabaseFailoverUnderLoad. Each
// syncs over HTTP discovery, one sync at a time, every failoverSyncEvery,
// through the apps in turn, as a load balancer would spread them: its k-th sync
// carries k as its uptime, which the store keeps, so that the stored uptime
// says which of its syncs landed last. The agents' syncs are spread over the
// interval.
type agentLoad struct {
	ids  []string
	stop chan struct{}
	halt func()

	mu sync.Mutex
	// attempts are each agent's syncs, in order.
	attempts [][]syncAttempt
}

// startAgentLoad starts failoverAgents agents syncing through nodes, until
// halt, or the end of the test.
func startAgentLoad(t *testing.T, nodes []*clusterNode) *agentLoad {
	t.Helper()
	l := &agentLoad{stop: make(chan struct{}), attempts: make([][]syncAttempt, failoverAgents)}
	client := &http.Client{Timeout: 10 * time.Second}
	var done sync.WaitGroup
	for a := range failoverAgents {
		id := fmt.Sprintf("agent-failover-%d", a)
		l.ids = append(l.ids, id)
		done.Go(func() {
			// The agents start a share of the interval apart: a deliberate delay,
			// which spreads their syncs over it.
			select {
			case <-l.stop:
				return
			case <-time.After(time.Duration(a) * failoverSyncEvery / failoverAgents):
			}
			tick := time.NewTicker(failoverSyncEvery)
			defer tick.Stop()
			for seq := int64(1); ; seq++ {
				attempt := syncThrough(client, nodes[(a+int(seq))%len(nodes)], id, seq)
				l.mu.Lock()
				l.attempts[a] = append(l.attempts[a], attempt)
				l.mu.Unlock()
				select {
				case <-l.stop:
					return
				case <-tick.C:
				}
			}
		})
	}
	l.halt = sync.OnceFunc(func() {
		close(l.stop)
		done.Wait()
	})
	t.Cleanup(l.halt)
	return l
}

// syncThrough sends the sync of agent id with sequence seq to via's HTTP
// discovery listener, with the agent's token, which its first sync enrolls.
func syncThrough(client *http.Client, via *clusterNode, id string, seq int64) syncAttempt {
	attempt := syncAttempt{via: via, seq: seq, sent: time.Now()}
	body := fmt.Sprintf(`{"id":%q,"endpoint":"http://127.0.0.1:1","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1,"uptimeSeconds":%d}`, id, seq)
	r, err := http.NewRequest(http.MethodPost, "http://"+via.cfg.HTTPDiscovery.Addr+"/api/v1/discovery/sync", strings.NewReader(body))
	if err != nil {
		return attempt
	}
	r.Header.Set("Content-Type", jsonType)
	r.Header.Set("Authorization", "Bearer token-of-"+id)
	w, err := client.Do(r)
	attempt.answered = time.Now()
	if err != nil {
		return attempt
	}
	_, _ = io.Copy(io.Discard, w.Body)
	_ = w.Body.Close()
	attempt.status = w.StatusCode
	return attempt
}

// snapshot returns a copy of every agent's syncs so far.
func (l *agentLoad) snapshot() [][]syncAttempt {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]syncAttempt, len(l.attempts))
	for a, attempts := range l.attempts {
		out[a] = slices.Clone(attempts)
	}
	return out
}

// every reports whether each agent has a sync that ok holds for.
func (l *agentLoad) every(ok func(syncAttempt) bool) bool {
	for _, attempts := range l.snapshot() {
		if !slices.ContainsFunc(attempts, ok) {
			return false
		}
	}
	return true
}

// answeredOK reports whether at answered 200 a sync sent after since.
func answeredOK(at syncAttempt, since time.Time) bool {
	return at.status == http.StatusOK && at.sent.After(since)
}

// leaderzStepDown polls the /leaderz of n, which led at since, every 5 ms until
// it answers 503, and returns when the last request it answered 200 was sent,
// since itself when none was, and when the 503 was answered. It fails the test
// when no 503 comes within limit of since, saying it wants one within want.
func leaderzStepDown(t *testing.T, n *clusterNode, since time.Time, want, limit time.Duration) (lastLeading, stepDownSeen time.Time) {
	t.Helper()
	lastLeading = since
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		sent := time.Now()
		status, _, err := get(n.app.ops.Addr(), "/leaderz", nil)
		switch {
		case err == nil && status == http.StatusServiceUnavailable:
			return lastLeading, time.Now()
		case err == nil && status == http.StatusOK:
			lastLeading = sent
		}
		if time.Since(since) > limit {
			t.Fatalf("%s's /leaderz answers %d, %v, %s on, want 503 within %s", n.id, status, err, time.Since(since), want)
		}
		<-tick.C
	}
}

// Three apps of the database mode run on one database, each on a loopback IP
// of its own with the same ports, and each through a TCP proxy of its own,
// which every pool of the app and its events listener connect through.
// Simulated agents sync over HTTP discovery through all three, the followers
// forwarding to the leader, each agent one sync at a time, its sequence carried
// as its uptime. While they sync, at a random point of the leader's renewal
// interval, the leader's proxy cuts its connections and refuses new ones: the
// leader is cut off the database, as a partition cuts it, not stopped, which
// would release the lease. Measured from the cut:
//
//   - the cut-off leader steps down within two thirds of the lease of its last
//     renewal's send, and so of the cut: its /leaderz answers 503 within that;
//     the work of its leader-only runners, which they run under WhenLeader,
//     ends within that and workEndTolerance, the time the runners take to
//     return once the step-down cancelled them, never to start again, and
//     before the new leader's starts;
//   - the first sync sent after the cut that is answered 200, through the new
//     leader, is answered within the lease and one renewal interval (a renewal
//     in flight at the cut may still commit, and extend the lease from the
//     cut); no sync sent to the cut-off replica after the cut is answered 200;
//   - every agent's syncs sent after the takeover are answered 200, the
//     remaining follower forwarding its share to the new leader, and the
//     followers count their forwarded syncs;
//   - read from the database through a store of the test's own, every agent
//     holds at least the sequence of its last sync answered 200: no write that
//     was acknowledged is lost (a later sync that got no answer may have landed).
//
// No log line of any of the three carries the database URL or its
// credentials.
func TestDatabaseFailoverUnderLoad(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	hosts := loopbackIPs(t, 3)
	// The proxies bind their ports before the apps' shared ports are chosen.
	proxies := make([]*dbProxy, len(hosts))
	proxied := make([]string, len(hosts))
	for i := range hosts {
		proxies[i], proxied[i] = newDBProxy(t, databaseURL)
	}
	nodes := databaseNodes(t, databaseURL, hosts...)
	proxyOf := map[*clusterNode]*dbProxy{}
	work := map[*clusterNode]*leaderWork{}
	var secrets []string
	for i, n := range nodes {
		n.cfg.Cluster.Database.URL = proxied[i]
		n.cfg.Cluster.Database.Lease = failoverLease
		proxyOf[n], work[n] = proxies[i], &leaderWork{}
		n.opts = append(n.opts, withLeaderWork(work[n].watch))
		secrets = append(secrets, urlSecrets(t, proxied[i])...)
	}
	for _, n := range nodes {
		n.start(t)
	}
	serving(t, nodes...)
	old, followers := databaseLeader(t, nodes)
	waitFor(t, 10*time.Second, old.id+" running the work of its leader-only runners", func() bool {
		return work[old].state().running == leaderOnlyRunners
	})

	load := startAgentLoad(t, nodes)
	waitFor(t, 10*time.Second, "every agent's syncs answered 200 twice", func() bool {
		for _, attempts := range load.snapshot() {
			if ok := slices.DeleteFunc(attempts, func(at syncAttempt) bool { return at.status != http.StatusOK }); len(ok) < 2 {
				return false
			}
		}
		return true
	})
	for _, f := range followers {
		if v, ok := series(scrapeOps(t, f.app.ops.Addr()), forwardedSyncs); !ok || v < 1 {
			t.Fatalf("%s forwarded %v discovery syncs before the cut, %v; want one at least", f.id, v, ok)
		}
	}

	// The cut lands at a random point of the leader's renewal interval, a third of
	// the lease, as a partition would, not at the same point in every run: a
	// deliberate delay, not a wait for a condition.
	time.Sleep(rand.N(failoverLease / 3))
	atCut := work[old].state()
	cut := time.Now()
	proxyOf[old].cut()
	cutDone := time.Now()

	// The step-down: a /leaderz that answered 200 to a request sent later than
	// two thirds of the lease after the cut proves the leader led past it.
	stepDown := 2 * failoverLease / 3
	lastLeading, stepDownSeen := leaderzStepDown(t, old, cut, stepDown, failoverLease)
	if led := lastLeading.Sub(cut); led > stepDown {
		t.Errorf("%s's /leaderz answered 200 to a request sent %s after its cut, want the step-down within %s", old.id, led, stepDown)
	}
	// The work ends once its runners return from the step-down's cancellation.
	waitFor(t, 5*time.Second, old.id+"'s leader-only work ending", func() bool { return work[old].state().running == 0 })
	ended := work[old].state()
	if took := ended.ended.Sub(cut); took <= 0 || took > stepDown+workEndTolerance {
		t.Errorf("%s's leader-only work ended %s after its cut, want within %s and the scheduling tolerance of %s", old.id, took, stepDown, workEndTolerance)
	}

	survivors := slices.DeleteFunc(slices.Clone(nodes), func(n *clusterNode) bool { return n == old })
	next, rest := databaseLeader(t, survivors)
	tookOver := time.Now()
	via := rest[0]
	forwardedAtTakeover, _ := series(scrapeOps(t, via.app.ops.Addr()), forwardedSyncs)
	waitFor(t, 10*time.Second, "every agent's sync sent after the takeover answered 200, one through "+via.id, func() bool {
		sentAfter := func(at syncAttempt) bool { return answeredOK(at, tookOver) }
		through := func(at syncAttempt) bool { return at.via == via && answeredOK(at, tookOver) }
		return load.every(sentAfter) && slices.ContainsFunc(slices.Concat(load.snapshot()...), through)
	})
	load.halt()
	if v, ok := series(scrapeOps(t, via.app.ops.Addr()), forwardedSyncs); !ok || v <= forwardedAtTakeover {
		t.Errorf("%s forwarded %v discovery syncs, %v, and %v at the takeover: want more, to %s", via.id, v, ok, forwardedAtTakeover, next.id)
	}

	attempts := load.snapshot()
	var first syncAttempt
	for _, at := range slices.Concat(attempts...) {
		if !answeredOK(at, cut) {
			continue
		}
		if at.via == old && at.sent.After(cutDone) {
			t.Errorf("a sync sent to %s %s after its cut answered 200: a replica cut off the database wrote", old.id, at.sent.Sub(cut))
		}
		if first.answered.IsZero() || at.answered.Before(first.answered) {
			first = at
		}
	}
	if first.via == nil {
		t.Fatal("no sync sent after the cut answered 200")
	}
	resumed := first.answered.Sub(cut)
	if bound := failoverLease + failoverLease/3; resumed > bound {
		t.Errorf("the first sync sent after the cut answered 200 %s after it, through %s, want within %s, the lease and one renewal interval", resumed, first.via.id, bound)
	}

	waitFor(t, 10*time.Second, next.id+" running the work of its leader-only runners", func() bool {
		return work[next].state().running == leaderOnlyRunners
	})
	if began := work[next].state().firstStart; !began.After(ended.ended) {
		t.Errorf("%s's leader-only work started %s before %s's ended", next.id, ended.ended.Sub(began), old.id)
	}
	if now := work[old].state(); now.started != atCut.started || now.running != 0 {
		t.Errorf("%s's leader-only work: %d calls running and %d started since the cut, want none", old.id, now.running, now.started-atCut.started)
	}

	ctx := context.Background()
	store, err := sqlstore.Open(ctx, sqlstore.Options{Primary: pgtest.Open(t, databaseURL), Dialect: sqlstore.Postgres{}})
	if err != nil {
		t.Fatalf("open a store on the database: %v", err)
	}
	for a, id := range load.ids {
		var acked, sent int64
		for _, at := range attempts[a] {
			sent = at.seq
			if at.status == http.StatusOK {
				acked = at.seq
			}
		}
		agent, err := store.GetAgent(ctx, id)
		if err != nil {
			t.Errorf("%s read from the database: %v", id, err)
			continue
		}
		if got := agent.UptimeSeconds(); got < acked || got > sent {
			t.Errorf("%s holds sequence %d in the database, want at least %d, its last sync answered 200, and at most %d, its last sent", id, got, acked, sent)
		}
	}

	texts := map[string]string{}
	for _, n := range nodes {
		texts[n.id+"'s log"] = n.logs.String()
	}
	noSecrets(t, secrets, texts)
	// What the agents' syncs sent after the cut were answered, by status (0: no
	// answer).
	statuses := map[int]int{}
	for _, at := range slices.Concat(attempts...) {
		if at.sent.After(cut) {
			statuses[at.status]++
		}
	}
	t.Logf("%s cut off the database: its last /leaderz 200 was sent %s after the cut and its first 503 answered %s after (bound %s), its leader-only work ended %s after (bound %s); %s took over, and the first sync sent after the cut answered 200 through %s %s after it (bound %s); the syncs sent after the cut, by status: %v",
		old.id, lastLeading.Sub(cut), stepDownSeen.Sub(cut), stepDown, ended.ended.Sub(cut), stepDown+workEndTolerance, next.id, first.via.id, resumed, failoverLease+failoverLease/3, statuses)
}
