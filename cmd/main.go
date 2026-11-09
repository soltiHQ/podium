package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/app"
	"github.com/soltiHQ/control-plane/internal/buildinfo"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/service/agent"
)

// shutdownTimeout caps how long [App.Shutdown] may take. If any cleanup
// step (Raft fsync, hub drain, pool close) hangs past this, the process
// exits dirty rather than holding the runtime forever.
const shutdownTimeout = 30 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, serve))
}

// usage is what -h prints, and what follows an error of the command line.
const usage = `usage: podium [--config PATH]
       podium agent-token [--generation N] [--] <agent-id>
       podium version

podium [--config PATH] runs the server; the config file is PATH, or CONFIG_PATH.
podium agent-token prints the token of an agent ID, derived from the enrollment secret.
podium version, or podium --version, prints the version.
`

// run is podium's command line: it runs the command of args and returns the exit
// code. serve starts the server with the path of the config file, "" for none.
// The command line is strict: an unknown flag or subcommand, a flag without its
// value or an argument that belongs to no command prints the error and the usage
// to stderr and exits 2, before anything starts or any configuration loads.
// -h and --help print the usage to stdout and exit 0.
func run(args []string, stdout, stderr io.Writer, serve func(configPath string) int) int {
	if len(args) > 0 {
		switch args[0] {
		case "agent-token":
			// The subcommand needs no configuration.
			return agentToken(args[1:], stdout, stderr)
		case "version":
			if len(args) > 1 {
				return usageError(stderr, errors.New("version takes no arguments"))
			}
			return printVersion(stdout, stderr)
		}
	}
	var (
		path    string
		version bool
		fs      = flag.NewFlagSet("podium", flag.ContinueOnError)
	)
	fs.SetOutput(io.Discard)
	fs.StringVar(&path, "config", "", "")
	fs.BoolVar(&version, "version", false, "")
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		_, _ = io.WriteString(stdout, usage)
		return 0
	case err != nil:
		return usageError(stderr, err)
	}
	if fs.NArg() > 0 {
		arg := fs.Arg(0)
		if fs.NArg() == len(args) {
			return usageError(stderr, fmt.Errorf("unknown subcommand %q", arg))
		}
		return usageError(stderr, fmt.Errorf("unexpected argument %q", arg))
	}
	if version {
		return printVersion(stdout, stderr)
	}
	return serve(path)
}

// usageError prints err and the usage to stderr and returns 2.
func usageError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "podium: %v\n%s", err, usage)
	return 2
}

// printVersion prints the version line.
func printVersion(stdout, stderr io.Writer) int {
	if _, err := fmt.Fprintln(stdout, buildinfo.Read()); err != nil {
		_, _ = fmt.Fprintf(stderr, "podium: write the version: %v\n", err)
		return 1
	}
	return 0
}

// serve runs the server with the config file at configPath ("" for CONFIG_PATH
// or none) and returns the exit code. Until the config is read it logs through a
// bootstrap logger: JSON on stdout at info; then it logs as log.level and
// log.format say.
func serve(configPath string) int {
	bootstrap := zerolog.New(os.Stdout).Level(zerolog.InfoLevel).With().Timestamp().Logger()
	logStart(bootstrap, buildinfo.Read())

	cfg, err := config.Load(configPath, bootstrap)
	if err != nil {
		bootstrap.Error().Err(err).Msg("failed to load config")
		return 1
	}
	// From here on podium logs as log.level and log.format say.
	logger := newLogger(cfg.Log, os.Stdout)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, logger)
	if err != nil {
		logger.Error().Err(err).Msg("failed to build app")
		return 1
	}

	runErr := a.Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := a.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		logger.Error().Err(shutdownErr).Dur("timeout", shutdownTimeout).Msg("shutdown failed")
	}

	if code := exitCode(runErr, shutdownErr); code != 0 {
		logger.Error().Err(runErr).Msg("server exited with error")
		return code
	}
	logger.Info().Msg("server stopped")
	return 0
}

// newLogger builds the logger of a run from the log settings, which
// [config.LogConfig.Validate] has checked: json is zerolog's JSON, text its
// console writer without colour.
func newLogger(cfg config.LogConfig, w io.Writer) zerolog.Logger {
	level, err := zerolog.ParseLevel(cfg.Level)
	if err != nil {
		level = zerolog.InfoLevel
	}
	if cfg.Format == "text" {
		w = zerolog.ConsoleWriter{Out: w, NoColor: true, TimeFormat: time.RFC3339}
	}
	return zerolog.New(w).Level(level).With().Timestamp().Logger()
}

// logStart writes the first line of a run: which podium this is.
func logStart(logger zerolog.Logger, info buildinfo.Info) {
	event := logger.Info().
		Str("version", info.Version).
		Str("commit", info.Commit).
		Bool("modified", info.Modified)
	if !info.BuildTime.IsZero() {
		event = event.Time("built", info.BuildTime)
	}
	event.Str("go_version", info.GoVersion).
		Str("platform", info.Platform).
		Msg("podium starting")
}

// Signal cancellation is a clean shutdown; failed cleanup or runner errors are
// visible to a supervisor through a nonzero exit status.
func exitCode(runErr, shutdownErr error) int {
	if shutdownErr != nil {
		return 1
	}
	if runErr != nil && runErr != context.Canceled {
		return 1
	}
	return 0
}

// agentTokenUsage is the usage of "podium agent-token".
const agentTokenUsage = "usage: podium agent-token [--generation N] [--] <agent-id>"

// agentToken runs "podium agent-token [--generation N] [--] <agent-id>": it
// prints the token of that agent ID at generation N, 0 by default, derived from
// the enrollment secret (agent.DeriveToken), and a newline, and returns the exit
// code. Generation 0 is the token the agent ID enrolls with; a derived
// credential names the generation of its token. It reads the secret from
// config.EnrollSecretEnv only and applies the checks of the server: the secret
// has at least agent.MinEnrollSecretBytes bytes and the ID passes
// model.ValidateAgentID. A failure prints one line to stderr, never the secret,
// and nothing to stdout. A bare -h or --help prints the usage to stdout.
// --generation N, or --generation=N, comes before the agent ID, N a whole
// number from 0 to 4294967295. "--" ends the options, for an agent ID that
// starts with "-".
func agentToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		_, _ = fmt.Fprintf(stdout, "%s\nPrints the token of <agent-id> at generation N (default 0, the token it enrolls with), derived from %s.\n", agentTokenUsage, config.EnrollSecretEnv)
		return 0
	}
	generation, args, err := agentTokenOptions(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "podium: %v; %s\n", err, agentTokenUsage)
		return 2
	}
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, agentTokenUsage)
		return 2
	}
	id := args[0]
	if err := model.ValidateAgentID(id); err != nil {
		_, _ = fmt.Fprintf(stderr, "podium: %v\n", err)
		return 1
	}
	secret := os.Getenv(config.EnrollSecretEnv)
	switch {
	case secret == "":
		_, _ = fmt.Fprintf(stderr, "podium: %s is not set\n", config.EnrollSecretEnv)
		return 1
	case len(secret) < agent.MinEnrollSecretBytes:
		_, _ = fmt.Fprintf(stderr, "podium: %s must have at least %d bytes\n", config.EnrollSecretEnv, agent.MinEnrollSecretBytes)
		return 1
	}
	if _, err := fmt.Fprintln(stdout, agent.DeriveToken(secret, id, generation)); err != nil {
		_, _ = fmt.Fprintf(stderr, "podium: write the token: %v\n", err)
		return 1
	}
	return 0
}

// agentTokenOptions reads the options of "podium agent-token" off the front of
// args: --generation N or --generation=N, and "--", which ends them. It returns
// the generation, 0 when none is given, and the arguments after the options.
func agentTokenOptions(args []string) (uint32, []string, error) {
	var generation uint32
	for len(args) > 0 {
		var value string
		switch arg := args[0]; {
		case arg == "--":
			return generation, args[1:], nil
		case arg == "--generation":
			if len(args) < 2 {
				return 0, nil, errors.New("--generation needs a value")
			}
			value, args = args[1], args[2:]
		case strings.HasPrefix(arg, "--generation="):
			value, args = strings.TrimPrefix(arg, "--generation="), args[1:]
		default:
			return generation, args, nil
		}
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return 0, nil, fmt.Errorf("--generation %q is not a whole number from 0 to 4294967295", value)
		}
		generation = uint32(n)
	}
	return generation, args, nil
}
