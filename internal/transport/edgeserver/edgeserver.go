package edgeserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	discoverv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/edgeserver/handlers"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

// EdgeServer is a gRPC and HTTP edge server.
type EdgeServer struct {
	http *http.Server
	grpc *grpc.Server

	logger  zerolog.Logger
	storage storage.Storage

	grpcAddr string
}

// NewEdgeServer creates a new edge server instance.
func NewEdgeServer(cfg Config, logger zerolog.Logger, storage storage.Storage) *EdgeServer {
	logger = logger.Level(cfg.logLevel)

	s := &EdgeServer{
		storage: storage,
		logger:  logger.With().Str("server", "edge").Logger(),
	}
	if cfg.addrHTTP != "" {
		var (
			handler = handlers.NewHttp(logger, storage)
			mux     = http.NewServeMux()
		)
		mux.HandleFunc("POST /v1/sync", handler.Sync)
		s.http = &http.Server{
			ReadHeaderTimeout: cfg.configHTTP.Timeouts.ReadHeader,
			ReadTimeout:       cfg.configHTTP.Timeouts.Read,
			WriteTimeout:      cfg.configHTTP.Timeouts.Write,
			IdleTimeout:       cfg.configHTTP.Timeouts.Idle,
			Addr:              cfg.addrHTTP,
			Handler:           mux,
		}
	}
	if cfg.addrGRPC != "" {
		var (
			handler = handlers.NewGrpc(logger, storage)
			srv     = grpc.NewServer(
				grpc.MaxRecvMsgSize(cfg.configGRPC.Limits.MaxRecvMsgSize),
				grpc.MaxSendMsgSize(cfg.configGRPC.Limits.MaxSendMsgSize),
				grpc.ConnectionTimeout(cfg.configGRPC.ConnectionTimeout),
			)
		)
		discoverv1.RegisterDiscoverServiceServer(srv, handler)
		s.grpcAddr = cfg.addrGRPC
		s.grpc = srv
	}
	return s
}

// Run starts configured HTTP / gRPC endpoints and blocks until:
//   - context is canceled
//   - one of the servers returns a fatal error.
func (s *EdgeServer) Run(ctx context.Context) error {
	if s.http == nil && s.grpc == nil {
		s.logger.Warn().Msg("edge server: no endpoints configured; nothing to start")
		return nil
	}

	s.logger.Info().Msg("edge server: starting")
	errCh := make(chan error, 2)

	if s.http != nil {
		go s.runHTTP(errCh)
	}
	if s.grpc != nil {
		go s.runGRPC(errCh)
	}

	select {
	case <-ctx.Done():
		s.logger.Info().Msg("edge server: context cancelled, starting graceful shutdown")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.shutdown(shutdownCtx)
		return ctx.Err()

	case err := <-errCh:
		if err != nil {
			s.logger.Error().Err(err).Msg("edge server: transport terminated with error")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.shutdown(shutdownCtx)
			return err
		}
		s.logger.Info().Msg("edge server: transports stopped cleanly")
		return nil
	}
}

func (s *EdgeServer) runHTTP(errCh chan<- error) {
	s.logger.Info().
		Str("addr", s.http.Addr).
		Msg("starting HTTP endpoint")

	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("http listener error: %w", err)
		return
	}
	errCh <- nil
}

func (s *EdgeServer) runGRPC(errCh chan<- error) {
	lis, err := net.Listen("tcp", s.grpcAddr)
	if err != nil {
		errCh <- fmt.Errorf("grpc listener listen error: %w", err)
		return
	}
	s.logger.Info().
		Str("addr", s.grpcAddr).
		Msg("edge server: starting gRPC endpoint")

	if err = s.grpc.Serve(lis); err != nil {
		if errors.Is(err, grpc.ErrServerStopped) {
			errCh <- nil
			return
		}
		errCh <- fmt.Errorf("grpc listener serve error: %w", err)
		return
	}
	errCh <- nil
}

func (s *EdgeServer) shutdown(ctx context.Context) {
	if s.http != nil {
		s.logger.Info().Msg("edge server: HTTP graceful shutdown started")
		if err := s.http.Shutdown(ctx); err != nil {
			s.logger.Error().Err(err).
				Msg("edge server: HTTP graceful shutdown failed; forcing close")
			_ = s.http.Close()
		} else {
			s.logger.Info().Msg("edge server: HTTP graceful shutdown completed")
		}
	}

	if s.grpc != nil {
		s.logger.Info().Msg("edge server: stopping gRPC server")
		done := make(chan struct{})
		go func() {
			s.grpc.GracefulStop()
			close(done)
		}()
		select {
		case <-ctx.Done():
			s.logger.Warn().Msg("edge server: gRPC graceful stop timed out; forcing Stop()")
			s.grpc.Stop()
		case <-done:
			s.logger.Info().Msg("edge server: gRPC graceful stop completed")
		}
	}
}
