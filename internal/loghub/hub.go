// Package loghub fans out a single backing log stream (CP → agent) to
// N UI subscribers. The Hub maintains one underlying connection per
// (agentID, taskID) and closes it when the last subscriber leaves.
//
// Slow-subscriber policy: per-subscriber buffer; on overflow events are
// dropped and a Lagged sentinel is enqueued so the client can render
// "N events skipped" without falling further behind the source.
package loghub

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/rs/zerolog"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
)

// ErrSourceClosed is returned by Subscribe when the underlying source has
// already finished (e.g. the agent stream ended between the lookup and the
// subscriber registration). Callers should treat it like a brief, retryable
// "not available" — usually a fresh Subscribe will reopen.
var ErrSourceClosed = errors.New("loghub: source closed")

// OpenFunc opens a backing log stream for a given (agentID, taskID).
// The returned channel must be closed by the producer when the stream ends.
type OpenFunc func(ctx context.Context, agentID, taskID string) (<-chan *taskv1.StreamTaskLogsResponse, error)

// Options tunes Hub behaviour. Zero values use safe defaults.
type Options struct {
	// SubscriberBuffer is the per-subscriber channel buffer size.
	// Larger = more tolerance for slow readers before Lagged kicks in.
	// Default: 64.
	SubscriberBuffer int
	// OpenTimeout bounds establishment without limiting a healthy stream lifetime.
	OpenTimeout time.Duration
	// Logger records recovered source panics. The zero value discards them.
	Logger zerolog.Logger
}

// Hub multiplexes one backing log stream per (agentID, taskID) over N
// subscribers.
type Hub struct {
	open OpenIdentityFunc
	opts Options

	mu      sync.Mutex
	sources map[key]*source
	closed  bool
}

type key struct {
	agentID string
	taskID  string
	taskUID string
}

// New builds a Hub. open is the factory that produces a backing stream
// when no source for the given (agentID, taskID) exists.
type OpenIdentityFunc func(context.Context, string, string, string) (<-chan *taskv1.StreamTaskLogsResponse, error)

func New(open OpenFunc, opts Options) *Hub {
	return NewIdentity(func(ctx context.Context, agentID, taskID, _ string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
		return open(ctx, agentID, taskID)
	}, opts)
}

func NewIdentity(open OpenIdentityFunc, opts Options) *Hub {
	if opts.OpenTimeout <= 0 {
		opts.OpenTimeout = 10 * time.Second
	}
	if opts.SubscriberBuffer <= 0 {
		opts.SubscriberBuffer = 64
	}
	return &Hub{
		open:    open,
		opts:    opts,
		sources: make(map[key]*source),
	}
}

// Subscribe returns a channel of events plus an unsubscribe function. The
// channel is closed by the Hub when either:
//
//   - The caller invokes unsubscribe (typically via defer), AND no other
//     subscribers remain — in that case the backing stream is also torn down.
//   - The backing agent stream ends naturally — all subscribers' channels
//     close together.
//
// Slow readers receive a synthetic Lagged event when they fall behind
// the source, so they can render the gap without polluting later events.
//
// Racing a teardown: if the backing source is ending concurrently, Subscribe
// either returns ErrSourceClosed (the source was already torn down) or, more
// rarely, a valid channel that the Hub closes immediately with no events
// delivered. Callers should treat both as "retry": a fresh Subscribe reopens
// the source.
func (h *Hub) Subscribe(ctx context.Context, agentID, taskID string) (<-chan *taskv1.StreamTaskLogsResponse, func(), error) {
	return h.SubscribeIdentity(ctx, agentID, taskID, "")
}

func (h *Hub) SubscribeIdentity(ctx context.Context, agentID, taskID, taskUID string) (<-chan *taskv1.StreamTaskLogsResponse, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	k := key{agentID: agentID, taskID: taskID, taskUID: taskUID}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, nil, ErrSourceClosed
	}
	src, exists := h.sources[k]
	if !exists {
		sourceCtx, cancel := context.WithCancel(context.Background())
		src = &source{hub: h, key: k, ctx: sourceCtx, cancel: cancel, subs: make(map[*sub]struct{}), opened: make(chan struct{})}
		h.sources[k] = src
	}
	// Register before opening/pumping, so initial output cannot race registration.
	ch, unsub, err := src.addSub(ctx, h.opts.SubscriberBuffer)
	h.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if !exists {
		go src.openSource()
	}
	select {
	case <-src.opened:
		src.mu.Lock()
		openErr := src.openErr
		src.mu.Unlock()
		if openErr != nil {
			unsub()
			return nil, nil, openErr
		}
	case <-ctx.Done():
		unsub()
		return nil, nil, ctx.Err()
	case <-src.ctx.Done():
		unsub()
		return nil, nil, ErrSourceClosed
	}
	return ch, unsub, nil
}

func (s *source) openSource() {
	defer s.recoverPanic()
	timer := time.AfterFunc(s.hub.opts.OpenTimeout, s.cancel)
	stream, err := s.hub.open(s.ctx, s.key.agentID, s.key.taskID, s.key.taskUID)
	timer.Stop()
	if err == nil && s.ctx.Err() != nil {
		err = s.ctx.Err()
	}
	s.mu.Lock()
	s.openErr = err
	close(s.opened)
	s.mu.Unlock()
	if err != nil {
		s.shutdown()
		return
	}
	s.pump(stream)
}

// recoverPanic keeps a panic in the source goroutine (the open func or the
// pump) from crashing the process. The source shuts down: waiting subscribers
// get ErrSourceClosed and attached subscribers see their channel close.
func (s *source) recoverPanic() {
	rec := recover()
	if rec == nil {
		return
	}
	s.hub.opts.Logger.Error().
		Str("agent_id", s.key.agentID).
		Str("task_id", s.key.taskID).
		Str("panic", fmt.Sprintf("%v", rec)).
		Bytes("stack", debug.Stack()).
		Msg("log source panic recovered")
	s.shutdown()
}

// Close stops established and opening streams and rejects new subscriptions.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	sources := make([]*source, 0, len(h.sources))
	for _, s := range h.sources {
		sources = append(sources, s)
	}
	h.mu.Unlock()
	for _, s := range sources {
		s.shutdown()
	}
}

// source represents one backing log stream + its subscribers.
type source struct {
	hub     *Hub
	key     key
	cancel  context.CancelFunc
	ctx     context.Context
	opened  chan struct{}
	openErr error

	mu     sync.Mutex
	subs   map[*sub]struct{}
	closed bool
}

type sub struct {
	ch      chan *taskv1.StreamTaskLogsResponse
	done    chan struct{}
	dropped uint64 // pending Lagged count; guarded by source.mu
}

func (s *source) addSub(ctx context.Context, buffer int) (<-chan *taskv1.StreamTaskLogsResponse, func(), error) {
	sb := &sub{ch: make(chan *taskv1.StreamTaskLogsResponse, buffer), done: make(chan struct{})}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, ErrSourceClosed
	}
	s.subs[sb] = struct{}{}
	s.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			s.removeSub(sb)
		case <-sb.done:
		}
	}()
	return sb.ch, func() { s.removeSub(sb) }, nil
}

func (s *source) removeSub(sb *sub) {
	s.mu.Lock()
	if _, ok := s.subs[sb]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.subs, sb)
	close(sb.ch)
	close(sb.done)
	last := len(s.subs) == 0
	s.mu.Unlock()

	if last {
		s.shutdown()
	}
}

// pump reads the backing stream and broadcasts to subscribers until the
// stream closes (EOF or cancellation). Final shutdown closes any remaining
// subscriber channels.
func (s *source) pump(stream <-chan *taskv1.StreamTaskLogsResponse) {
	defer s.shutdown()
	for {
		select {
		case <-s.ctx.Done():
			return
		case ev, ok := <-stream:
			if !ok {
				return
			}
			s.broadcast(ev)
		}
	}
}

// broadcast sends ev to every subscriber, holding s.mu for the whole fan-out.
//
// The lock is held during the sends — not just to snapshot the set — on
// purpose: removeSub/shutdown close subscriber channels under s.mu, so sending
// under the same lock makes "send" and "close" mutually exclusive. Releasing
// the lock before sending (the obvious optimization) races a concurrent
// unsubscribe and panics with "send on closed channel" — a closed channel is
// "ready" for send inside a select, so the default case does not save us.
// Holding the lock is cheap because every send is non-blocking.
//
// If a subscriber's buffer is full the event is dropped and a pending Lagged
// count is incremented; the next successful send injects a Lagged envelope
// first so the client can render the gap.
func (s *source) broadcast(ev *taskv1.StreamTaskLogsResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for sb := range s.subs {
		if sb.dropped > 0 {
			lagged := &taskv1.StreamTaskLogsResponse{
				Kind: &taskv1.StreamTaskLogsResponse_Lagged{Lagged: &taskv1.Lagged{Skipped: sb.dropped}},
			}
			select {
			case sb.ch <- lagged:
				sb.dropped = 0
			default:
				sb.dropped++
				continue
			}
		}
		select {
		case sb.ch <- ev:
		default:
			sb.dropped++
		}
	}
}

// shutdown cancels the backing ctx (closing the source's channel), removes
// the source from the Hub's index, and closes any subscriber channels that
// the caller didn't already release. Idempotent.
func (s *source) shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	subs := make([]*sub, 0, len(s.subs))
	for sb := range s.subs {
		subs = append(subs, sb)
	}
	s.subs = nil
	s.mu.Unlock()

	s.cancel()

	s.hub.mu.Lock()
	if cur, ok := s.hub.sources[s.key]; ok && cur == s {
		delete(s.hub.sources, s.key)
	}
	s.hub.mu.Unlock()

	for _, sb := range subs {
		close(sb.ch)
		close(sb.done)
	}
}
