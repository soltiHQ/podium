package httpserver

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/rs/zerolog"
)

// ErrorLog returns the [http.Server.ErrorLog] of the server called name: each
// line net/http reports on its own, such as a failed TLS handshake or the panic
// of a handler, is one WARN line of logger with the field runner. Without it
// those lines go to Go's text logger on stderr.
func ErrorLog(logger zerolog.Logger, name string) *log.Logger {
	return log.New(errorLogWriter{logger: logger.With().Str("runner", name).Logger()}, "", 0)
}

// errorLogWriter writes each write as a WARN line.
type errorLogWriter struct{ logger zerolog.Logger }

func (w errorLogWriter) Write(p []byte) (int, error) {
	w.logger.Warn().Msg(strings.TrimRight(string(p), "\r\n"))
	return len(p), nil
}

// state is where a Runner is in its life.
type state int

const (
	idle    state = iota // neither Start nor Stop has run
	serving              // Start has run: it bound the listener, or failed to
	stopped              // Stop ran before Start, which then never binds
)

// Runner is a server.Runner implementation for http.Server.
type Runner struct {
	logger  zerolog.Logger
	handler http.Handler
	cfg     Config

	// mu guards state, and the fields that Start sets before ready closes.
	mu        sync.Mutex
	state     state
	srv       *http.Server
	ln        net.Listener
	ready     chan struct{}
	cancelCtx context.CancelFunc
}

// New creates an HTTP server runner.
func New(cfg Config, logger zerolog.Logger, handler http.Handler) (*Runner, error) {
	if handler == nil {
		return nil, ErrNilHandler
	}

	cfg = cfg.withDefaults()
	if cfg.Addr == "" {
		return nil, ErrEmptyAddr
	}

	return &Runner{
		handler: handler,
		logger:  logger,
		cfg:     cfg,
		ready:   make(chan struct{}),
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

	// The lock is held until the server is built, so that a Stop that comes in
	// the meantime waits for it, and then shuts it down: Serve after Shutdown
	// returns at once, and closes the listener.
	ln, err := net.Listen("tcp", r.cfg.Addr)
	if err != nil {
		close(r.ready)
		r.mu.Unlock()
		return err
	}
	r.ln = ln

	// Stop cancels the base context of every connection when the requests in
	// flight outlast its deadline. It is made here, under the lock, so that Stop
	// sees its cancel: Serve would ask for it only later, on this goroutine.
	baseCtx, cancel := context.WithCancel(context.Background())
	r.cancelCtx = cancel

	tlsEnabled := r.cfg.TLSConfig != nil
	srv := &http.Server{
		Addr:      r.cfg.Addr,
		Handler:   r.handler,
		TLSConfig: r.cfg.TLSConfig,

		ReadHeaderTimeout: r.cfg.ReadHeaderTimeout,
		ReadTimeout:       r.cfg.ReadTimeout,
		WriteTimeout:      r.cfg.WriteTimeout,
		IdleTimeout:       r.cfg.IdleTimeout,
		MaxHeaderBytes:    r.cfg.MaxHeaderBytes,

		ErrorLog: ErrorLog(r.logger, r.cfg.Name),

		BaseContext: func(net.Listener) context.Context { return baseCtx },
		ConnContext: r.cfg.ConnContext,
	}
	if r.cfg.OnShutdown != nil {
		srv.RegisterOnShutdown(r.cfg.OnShutdown)
	}
	r.srv = srv
	close(r.ready)
	r.mu.Unlock()

	r.logger.Info().
		Str("runner", r.cfg.Name).
		Str("addr", r.cfg.Addr).
		Bool("tls", tlsEnabled).
		Msg("http server listening")

	if tlsEnabled {
		err = srv.ServeTLS(ln, "", "")
	} else {
		err = srv.Serve(ln)
	}
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Stop shuts the server down within the ctx deadline: the listener closes at
// once, the requests in flight finish, and idle connections close. A request
// that is still running when ctx ends is cut: its context is cancelled and its
// connection closed. A Stop before Start marks the runner stopped, so that Start
// never binds, and closes Bound.
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	if r.state == idle {
		r.state = stopped
		close(r.ready)
		r.mu.Unlock()
		return nil
	}
	srv, cancelCtx := r.srv, r.cancelCtx
	r.mu.Unlock()
	if srv == nil {
		return nil // Start could not bind, or a Stop ran before it
	}

	r.logger.Info().
		Str("runner", r.cfg.Name).
		Msg("http server shutdown requested")

	// Start sets the cancel with the server.
	defer cancelCtx()

	if err := srv.Shutdown(ctx); err != nil {
		cancelCtx()
		_ = srv.Close()
		r.logger.Warn().
			Err(err).
			Str("runner", r.cfg.Name).
			Msg("http server hard-closed (timeout)")
		return err
	}

	r.logger.Info().
		Str("runner", r.cfg.Name).
		Msg("http server stopped")
	return nil
}
