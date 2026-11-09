package httpserver

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"

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

// Runner is a server.Runner implementation for http.Server.
type Runner struct {
	logger  zerolog.Logger
	handler http.Handler
	cfg     Config

	srv       *http.Server
	ln        net.Listener
	ready     chan struct{}
	cancelCtx context.CancelFunc

	started atomic.Bool
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

// Bound returns a channel that closes once Start has tried to bind the listener.
func (r *Runner) Bound() <-chan struct{} { return r.ready }

// Addr returns the address the listener is bound to, and "" before it is bound
// or when it could not be.
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

// Start binds the listener and serves until Stop() shuts it down.
func (r *Runner) Start(_ context.Context) error {
	if !r.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}

	ln, err := net.Listen("tcp", r.cfg.Addr)
	if err != nil {
		close(r.ready)
		return err
	}
	r.ln = ln

	// Stop cancels the base context of every connection when the requests in
	// flight outlast its deadline. It is made here, before ready closes, so that
	// Stop, which waits for ready, sees its cancel: Serve would ask for it only
	// later, on this goroutine.
	baseCtx, cancel := context.WithCancel(context.Background())
	r.cancelCtx = cancel

	r.srv = &http.Server{
		Addr:    r.cfg.Addr,
		Handler: r.handler,

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
		r.srv.RegisterOnShutdown(r.cfg.OnShutdown)
	}
	close(r.ready)

	tlsEnabled := r.cfg.TLSConfig != nil
	r.logger.Info().
		Str("runner", r.cfg.Name).
		Str("addr", r.cfg.Addr).
		Bool("tls", tlsEnabled).
		Msg("http server listening")

	if tlsEnabled {
		r.srv.TLSConfig = r.cfg.TLSConfig
		err = r.srv.ServeTLS(ln, "", "")
	} else {
		err = r.srv.Serve(ln)
	}
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Stop shuts the server down within the ctx deadline: the listener closes at
// once, the requests in flight finish, and idle connections close. A request
// that is still running when ctx ends is cut: its context is cancelled and its
// connection closed.
func (r *Runner) Stop(ctx context.Context) error {
	if !r.started.Load() {
		return nil
	}

	select {
	case <-r.ready:
	case <-ctx.Done():
		return ctx.Err()
	}

	srv := r.srv
	if srv == nil {
		return nil
	}

	r.logger.Info().
		Str("runner", r.cfg.Name).
		Msg("http server shutdown requested")

	if r.cancelCtx != nil {
		defer r.cancelCtx()
	}

	if err := srv.Shutdown(ctx); err != nil {
		if r.cancelCtx != nil {
			r.cancelCtx()
		}
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
