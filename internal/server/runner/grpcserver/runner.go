package grpcserver

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

// state is where a Runner is in its life.
type state int

const (
	idle    state = iota // neither Start nor Stop has run
	serving              // Start has run: it bound the listener, or failed to
	stopped              // Stop ran before Start, which then never binds
)

// Runner is a server.Runner implementation for grpc.Server.
type Runner struct {
	logger zerolog.Logger
	cfg    Config

	srv *grpc.Server

	// mu guards state, and ln, which Start sets before ready closes.
	mu    sync.Mutex
	state state
	ln    net.Listener
	ready chan struct{}
}

// New creates a gRPC server runner.
//
// Srv must be a fully configured *grpc.Server (with interceptors, keepalive, etc.).
// The caller should do registration of services before passing srv here.
func New(cfg Config, logger zerolog.Logger, srv *grpc.Server) (*Runner, error) {
	cfg = cfg.withDefaults()

	if srv == nil {
		return nil, ErrNilServer
	}
	if cfg.Addr == "" {
		return nil, ErrEmptyAddr
	}

	return &Runner{
		logger: logger,
		cfg:    cfg,
		srv:    srv,
		ready:  make(chan struct{}),
	}, nil
}

// Name returns the runner name.
func (r *Runner) Name() string { return r.cfg.Name }

// Bound returns a channel that closes once Start has tried to bind the listener,
// or once a Stop that came before Start has run.
func (r *Runner) Bound() <-chan struct{} { return r.ready }

// Addr returns the address the listener is bound to, and "" before it is bound,
// when it could not be, or when Stop came before Start.
func (r *Runner) Addr() string {
	select {
	case <-r.ready:
		if r.ln != nil {
			return r.ln.Addr().String()
		}
	default:
	}
	return ""
}

// Start binds the listener and serves until Stop() shuts it down. After a Stop
// that came first it returns nil at once and never binds: server.Run stops a
// runner whose goroutine has not reached Start yet when another runner exits.
func (r *Runner) Start(_ context.Context) error {
	r.mu.Lock()
	switch r.state {
	case serving:
		r.mu.Unlock()
		return ErrAlreadyStarted
	case stopped:
		r.mu.Unlock()
		return nil
	}
	r.state = serving

	// The lock is held until the listener is bound, so that a Stop that comes in
	// the meantime waits for it, and then stops the server: Serve after Stop or
	// GracefulStop returns at once, and closes the listener.
	ln, err := net.Listen(r.cfg.Network, r.cfg.Addr)
	if err != nil {
		close(r.ready)
		r.mu.Unlock()
		return err
	}
	r.ln = ln
	close(r.ready)
	r.mu.Unlock()

	r.logger.Info().
		Str("runner", r.cfg.Name).
		Str("addr", r.cfg.Addr).
		Msg("grpc server listening")

	err = r.srv.Serve(ln)
	if err == nil || errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// Stop attempts graceful shutdown and falls back to hard stop when ctx expires.
// A Stop before Start marks the runner stopped, so that Start never binds, and
// closes Bound.
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	switch r.state {
	case idle:
		r.state = stopped
		close(r.ready)
		r.mu.Unlock()
		return nil
	case stopped:
		r.mu.Unlock()
		return nil // a Stop ran before Start: nothing serves
	}
	r.mu.Unlock()

	srv := r.srv

	r.logger.Info().
		Str("runner", r.cfg.Name).
		Msg("grpc server shutdown requested")

	if r.cfg.OnShutdown != nil {
		r.cfg.OnShutdown()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.GracefulStop()
	}()

	select {
	case <-done:
		r.logger.Info().
			Str("runner", r.cfg.Name).
			Msg("grpc server stopped")
		return nil
	case <-ctx.Done():
		srv.Stop()
		r.logger.Warn().
			Str("runner", r.cfg.Name).
			Msg("grpc server hard-stopped (timeout)")
		return ctx.Err()
	}
}
