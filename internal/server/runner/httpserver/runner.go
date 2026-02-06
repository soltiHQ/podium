package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/rs/zerolog"
)

// Runner is a server.Runner implementation for http.Server.
type Runner struct {
	logger  zerolog.Logger
	handler http.Handler
	cfg     Config

	srv *http.Server
	ln  net.Listener

	started atomic.Bool
}

// New creates an HTTP server runner.
func New(cfg Config, logger zerolog.Logger, handler http.Handler) (*Runner, error) {
	if handler == nil {
		return nil, errors.New("httpserver: nil handler")
	}

	cfg = cfg.withDefaults()
	if cfg.Addr == "" {
		return nil, errors.New("httpserver: empty addr")
	}
	r := &Runner{
		handler: handler,
		logger:  logger,
		cfg:     cfg,
	}
	return r, nil
}

// Name returns the runner name.
func (r *Runner) Name() string { return r.cfg.Name }

// Start binds the listener and serves until ctx is canceled or Stop() shuts it down.
func (r *Runner) Start(_ context.Context) error {
	if !r.started.CompareAndSwap(false, true) {
		return errors.New("httpserver: already started")
	}

	ln, err := net.Listen("tcp", r.cfg.Addr)
	if err != nil {
		return err
	}
	r.ln = ln

	r.srv = &http.Server{
		Addr:    r.cfg.Addr,
		Handler: r.handler,

		ReadHeaderTimeout: r.cfg.ReadHeaderTimeout,
		ReadTimeout:       r.cfg.ReadTimeout,
		WriteTimeout:      r.cfg.WriteTimeout,
		IdleTimeout:       r.cfg.IdleTimeout,

		BaseContext: r.cfg.BaseContext,
		ConnContext: r.cfg.ConnContext,
	}

	r.logger.Info().
		Str("runner", r.cfg.Name).
		Str("addr", r.cfg.Addr).
		Msg("http server listening")

	err = r.srv.Serve(ln)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Stop gracefully shuts down the server within the ctx deadline.
func (r *Runner) Stop(ctx context.Context) error {
	srv := r.srv
	if srv == nil {
		return nil
	}

	r.logger.Info().
		Str("runner", r.cfg.Name).
		Msg("http server shutdown requested")

	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		return err
	}
	return nil
}
