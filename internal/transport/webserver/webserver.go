package webserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/internal/transport/middleware"
)

// WebServer is an HTTP UI server.
type WebServer struct {
	http *http.Server

	logger zerolog.Logger
	cfg    Config
	render *renderer
}

// NewWebServer creates a new web UI server instance.
func NewWebServer(cfg Config, logger zerolog.Logger) *WebServer {
	logger = logger.Level(cfg.logLevel)

	r, err := newRenderer(logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("web server: renderer init failed")
	}
	s := &WebServer{
		logger: logger.With().Str("server", "web").Logger(),
		cfg:    cfg,
		render: r,
	}
	if cfg.addrHTTP != "" {
		s.http = &http.Server{
			Addr:              cfg.addrHTTP,
			ReadHeaderTimeout: cfg.configHTTP.Timeouts.ReadHeader,
			ReadTimeout:       cfg.configHTTP.Timeouts.Read,
			WriteTimeout:      cfg.configHTTP.Timeouts.Write,
			IdleTimeout:       cfg.configHTTP.Timeouts.Idle,

			Handler: middleware.HttpChain(
				s.router(),
				logger,
				cfg.configHTTP.Middleware,
			),
		}
	}
	return s
}

// Run starts the configured HTTP endpoint and blocks until:
//   - the context is canceled, or
//   - the HTTP server returns a fatal error.
func (s *WebServer) Run(ctx context.Context) error {
	if s.http == nil {
		s.logger.Warn().Msg("web server: no endpoints configured; nothing to start")
		return nil
	}

	s.logger.Info().Msg("web server: starting")
	errCh := make(chan error, 1)

	go s.runHTTP(errCh)
	select {
	case <-ctx.Done():
		s.logger.Info().Msg("web server: context cancelled, starting graceful shutdown")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.shutdown(shutdownCtx)
		return nil

	case err := <-errCh:
		if err != nil {
			s.logger.Error().Err(err).Msg("web server: transport terminated with error")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.shutdown(shutdownCtx)
			return err
		}
		s.logger.Info().Msg("web server: HTTP server stopped cleanly")
		return nil
	}
}

func (s *WebServer) runHTTP(errCh chan<- error) {
	s.logger.Info().
		Str("addr", s.http.Addr).
		Msg("starting HTTP endpoint")

	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("http listener error: %w", err)
		return
	}
	errCh <- nil
}

func (s *WebServer) shutdown(ctx context.Context) {
	if s.http != nil {
		s.logger.Info().Msg("web server: HTTP graceful shutdown started")
		if err := s.http.Shutdown(ctx); err != nil {
			s.logger.Error().Err(err).
				Msg("web server: HTTP graceful shutdown failed; forcing close")
			_ = s.http.Close()
		} else {
			s.logger.Info().Msg("web server: HTTP graceful shutdown completed")
		}
	}
}
