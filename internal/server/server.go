// Package server provides unified lifecycle management for runtime components.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rs/zerolog"
)

type runnerExit struct {
	name string
	err  error
}

// Server orchestrates the lifecycle of multiple runners.
type Server struct {
	logger zerolog.Logger
	cfg    Config

	runners      []Runner
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

// New creates a Server.
func New(cfg Config, logger zerolog.Logger, runners ...Runner) (*Server, error) {
	if len(runners) == 0 {
		return nil, ErrNoRunners
	}
	cfg = cfg.withDefaults()

	var (
		seen = make(map[string]struct{}, len(runners))
		rs   = make([]Runner, 0, len(runners))
	)
	for _, r := range runners {
		if r == nil {
			return nil, ErrNilRunner
		}
		name := strings.TrimSpace(r.Name())
		if name == "" {
			return nil, fmt.Errorf("server: runner has empty name: %w", ErrEmptyRunnerName)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateRunnerName, name)
		}
		seen[name] = struct{}{}
		rs = append(rs, r)
	}
	return &Server{
		runners:      rs,
		cfg:          cfg,
		logger:       logger,
		shutdownDone: make(chan struct{}),
	}, nil
}

// Run starts all runners.
func (s *Server) Run(ctx context.Context) error {
	var (
		runCtx, runCancel = context.WithCancel(ctx)

		exitCh = make(chan runnerExit, 1)
		wg     sync.WaitGroup
		e      error
	)
	defer runCancel()

	wg.Add(len(s.runners))
	for _, r := range s.runners {
		r := r
		go func() {
			defer wg.Done()

			s.logger.Info().
				Str("runner", r.Name()).
				Msg("runner starting")

			err := r.Start(runCtx)
			select {
			case exitCh <- runnerExit{name: r.Name(), err: err}:
			default:
			}

			if err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Error().
					Err(err).
					Str("runner", r.Name()).
					Msg("runner exited")
				return
			}
			s.logger.Info().
				Str("runner", r.Name()).
				Msg("runner exited")
		}()
	}

	select {
	case <-ctx.Done():
		e = ctx.Err()
	case ex := <-exitCh:
		if ex.err == nil {
			e = &RunnerExitedError{Runner: ex.name}
		} else if errors.Is(ex.err, context.Canceled) {
			e = ex.err
		} else {
			e = &RunnerError{Runner: ex.name, Phase: "start", Err: ex.err}
		}
	}
	runCancel()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancelShutdown()
	shutdownErr := s.Shutdown(shutdownCtx)
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-shutdownCtx.Done():
		shutdownErr = errors.Join(shutdownErr, shutdownCtx.Err())
	}

	if shutdownErr != nil {
		return errors.Join(e, shutdownErr)
	}
	return e
}

// Shutdown gracefully stops all runners.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		go func() {
			defer close(s.shutdownDone)
			stopCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
			defer cancel()
			results := make(chan error, len(s.runners))
			for _, runner := range s.runners {
				go func() {
					err := runner.Stop(stopCtx)
					if err != nil {
						err = &RunnerError{Runner: runner.Name(), Phase: "stop", Err: err}
					}
					results <- err
				}()
			}
			var errs []error
			for range s.runners {
				select {
				case err := <-results:
					if err != nil {
						errs = append(errs, err)
					}
				case <-stopCtx.Done():
					s.shutdownErr = errors.Join(append(errs, stopCtx.Err())...)
					return
				}
			}
			s.shutdownErr = errors.Join(errs...)
		}()
	})
	select {
	case <-s.shutdownDone:
		return s.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
