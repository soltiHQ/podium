package raft

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// configCheckName is the name of the configuration check's runner.
const configCheckName = "config-check"

// The pauses between the fetches of one check that fail: the first, which
// doubles after each further failure up to the last.
const (
	configRetryPause    = time.Second
	configMaxRetryPause = time.Minute
)

// What the configuration check logs: one WARN for a check whose first fetch
// failed, one ERROR for a leader whose digests differ, and a line for a leader
// whose digests match, at INFO when it ends a mismatch or a failed fetch logged
// before, at DEBUG otherwise. A mismatch logged before is logged again at
// DEBUG, or at ERROR when a failed fetch was logged since.
const (
	configFetchFailed = "configuration check: cannot fetch the leader's configuration digests: retrying after 1, 2, 4, 8, 16 and 32 s, then every 60 s, from 1 s again at each change of leader"
	configDiffers     = "configuration check: this node's configuration differs from the leader's in settings that every replica must share"
	configMatches     = "configuration check: this node's configuration matches the leader's"
)

// ErrConfigCheckStarted indicates a configuration check started twice.
var ErrConfigCheckStarted = errors.New("raft: configuration check already started")

// ConfigCheck is the runner of a node's configuration check: while another node
// leads, it fetches the leader's configuration digests over the raft port and
// compares them with this node's, group by group. It checks at its start, once a
// leader other than this node is known, and again at each change of the leader
// the node knows, so that a node whose settings drifted is caught as a follower
// of a leader that shares the settings, and as the leader of followers that do.
// A node that leads, or knows no leader, checks nothing.
//
// It logs the outcome under runner=config-check, with the node's ID, the
// leader's ID and, for a mismatch, the names of the groups that differ; never a
// digest or a value:
//   - a mismatch at ERROR, once per leader and digest set since the last match:
//     the same leader seen again with the same digests, a flap of leadership
//     through no leader included, logs it at DEBUG only, unless a failed fetch
//     was logged since, which logs it at ERROR again; a match clears what was
//     logged, so a later mismatch is an ERROR again;
//   - a match at DEBUG, or at INFO when it follows a mismatch or a failed fetch
//     logged since the last match, which it ends;
//   - a fetch that fails at WARN, once per check of a leader: the check retries
//     after 1, 2, 4, 8, 16 and 32 s, then every 60 s, until a fetch succeeds or
//     the leader changes, which ends the pause, and a check of the next leader
//     starts again from 1 s.
type ConfigCheck struct {
	self   string          // this node's ID
	own    cluster.Digests // this node's digests
	leader func() (id, address string)
	fetch  func(ctx context.Context, address string) (cluster.Digests, error)
	// watch makes the check's wake be woken, without blocking, at every change
	// of the leader the node knows, until the function it returns stops it.
	watch func(wake chan<- struct{}) (stop func())
	// after is the timer of a pause, time.After; tests replace it.
	after  func(time.Duration) <-chan time.Time
	logger zerolog.Logger

	stop     chan struct{} // closed by Stop
	done     chan struct{} // closed once Start returns
	started  atomic.Bool
	stopOnce sync.Once
}

// newConfigCheck returns the check of the node self, whose digests are own:
// leader gives the leader the node knows, fetch a peer's digests, and watch
// wakes the check at each change of leader.
func newConfigCheck(self string, own cluster.Digests, leader func() (string, string), fetch func(context.Context, string) (cluster.Digests, error), watch func(chan<- struct{}) func(), logger zerolog.Logger) *ConfigCheck {
	return &ConfigCheck{
		self:   self,
		own:    own,
		leader: leader,
		fetch:  fetch,
		watch:  watch,
		after:  time.After,
		logger: logger.With().Str("runner", configCheckName).Str("node_id", self).Logger(),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// ConfigCheck returns the configuration check of the node, which compares the
// node's own configuration digests (Config.ConfigDigests), those it answers its
// peers with, with its leader's, fetched as a probe dials. It returns nil for a
// node without digests.
func (a *Admin) ConfigCheck(logger zerolog.Logger) *ConfigCheck {
	p := a.p
	if p.cfg.ConfigDigests == nil {
		return nil
	}
	return newConfigCheck(p.cfg.NodeID, *p.cfg.ConfigDigests, a.Leader, func(ctx context.Context, address string) (cluster.Digests, error) {
		return fetchDigests(ctx, address, p.security)
	}, p.watchLeader, logger)
}

// watchLeader wakes wake, without blocking, at every change of the leader that
// the node knows, which hashicorp/raft observes as a LeaderObservation, until
// the function it returns deregisters the observer. The observer runs in raft's
// goroutine and never blocks it, as WhenLeader's does.
func (p *Profile) watchLeader(wake chan<- struct{}) func() {
	observer := hraft.NewObserver(nil, false, func(o *hraft.Observation) bool {
		if _, ok := o.Data.(hraft.LeaderObservation); ok {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
		return false
	})
	p.raft.RegisterObserver(observer)
	return func() { p.raft.DeregisterObserver(observer) }
}

// Name implements server.Runner.
func (*ConfigCheck) Name() string { return configCheckName }

// Start implements server.Runner: it checks until Stop is called or ctx ends,
// and then returns nil.
func (c *ConfigCheck) Start(ctx context.Context) error {
	if !c.started.CompareAndSwap(false, true) {
		return ErrConfigCheckStarted
	}
	defer close(c.done)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	// The watch starts before the leader is read, so that no change slips in
	// between.
	wake := make(chan struct{}, 1)
	defer c.watch(wake)()
	c.logger.Info().Msg("runner started")
	c.run(ctx, wake)
	return nil
}

// Stop implements server.Runner: it ends Start and waits for it to return, or
// for ctx to end first.
func (c *ConfigCheck) Stop(ctx context.Context) error {
	if !c.started.Load() {
		return nil
	}
	c.stopOnce.Do(func() { close(c.stop) })
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// checkState is what the check logged that a later check depends on.
type checkState struct {
	// mismatches are the leaders whose digests a logged ERROR found to differ,
	// each with those digests, since the last match.
	mismatches map[string]cluster.Digests
	// warned says that a fetch that failed was logged since the last check
	// that compared digests.
	warned bool
}

// run checks the leader the node knows now, and again each time wake says it
// changed, until ctx ends.
func (c *ConfigCheck) run(ctx context.Context, wake <-chan struct{}) {
	var state checkState
	for {
		changed := false
		if id, address := c.leader(); id != "" && address != "" && id != c.self {
			changed = c.check(ctx, wake, id, address, &state)
		}
		if changed {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
	}
}

// check fetches the digests of the leader id, at address, and compares them
// with the node's. A fetch that fails logs one WARN and is tried again after a
// pause, which starts at configRetryPause and doubles after each failure up to
// configMaxRetryPause, until a fetch succeeds, ctx ends, or the leader changes,
// which check reports.
func (c *ConfigCheck) check(ctx context.Context, wake <-chan struct{}, id, address string, state *checkState) (changed bool) {
	pause := configRetryPause
	for attempt := 1; ; attempt++ {
		theirs, err := c.fetch(ctx, address)
		if err == nil {
			c.compare(id, theirs, state)
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		if attempt == 1 {
			c.logger.Warn().Err(err).Str("leader_id", id).Str("leader_address", address).Dur("retry_in", pause).Msg(configFetchFailed)
			state.warned = true
		}
		select {
		case <-ctx.Done():
			return false
		case <-wake:
			return true
		case <-c.after(pause):
		}
		pause = min(2*pause, configMaxRetryPause)
	}
}

// compare logs the outcome of the digests theirs of the leader id against the
// node's own, as the type's doc says.
func (c *ConfigCheck) compare(id string, theirs cluster.Digests, state *checkState) {
	groups := c.own.Differ(theirs)
	if len(groups) == 0 {
		event := c.logger.Debug()
		if len(state.mismatches) > 0 || state.warned {
			event = c.logger.Info()
		}
		event.Str("leader_id", id).Msg(configMatches)
		*state = checkState{}
		return
	}
	warned := state.warned
	state.warned = false
	if logged, ok := state.mismatches[id]; ok && logged == theirs && !warned {
		c.logger.Debug().Str("leader_id", id).Strs("groups", groups).Msg(configDiffers)
		return
	}
	c.logger.Error().Str("leader_id", id).Strs("groups", groups).Msg(configDiffers)
	if state.mismatches == nil {
		state.mismatches = map[string]cluster.Digests{}
	}
	state.mismatches[id] = theirs
}
