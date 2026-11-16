package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/database"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
)

// The database mode's intervals, which tests shorten through databaseTimings.
const (
	// startupRetry is how long a start retries a database it cannot reach,
	// before New fails and main exits 1, for the orchestrator to restart it.
	startupRetry = 30 * time.Second
	// startupPause is the pause between two attempts of a start.
	startupPause = time.Second
	// pingInterval is how often the primary is pinged for readiness, and
	// pingTimeout how long a ping waits for its answer.
	pingInterval = 5 * time.Second
	pingTimeout  = 2 * time.Second
)

// connectTimeout is the connect timeout of a database URL that sets none, or 0:
// a host that drops packets fails a connection within it, rather than within the
// operating system's TCP connect timeout, so that a replica that does not answer
// holds a listing that long before it falls back to the primary.
const connectTimeout = 5 * time.Second

// The application_name of each pool of the database mode, which names its
// connections in pg_stat_activity; the events listener's is podium-events.
const (
	primaryName    = "podium"
	replicaName    = "podium-replica"
	leaseName      = "podium-lease"
	migrateName    = "podium-migrate"
	dropSchemaName = "podium-drop-schema"
)

// What the database mode logs, besides the leadership and the events transport.
const (
	startRetrying = "database: the primary does not answer yet: retrying until the startup retry ends"
	pingFailed    = "database: the primary does not answer: /readyz reports storage failed until it does"
	pingRecovered = "database: the primary answers again: ready"
)

// databaseTimings are the intervals of the database mode that tests shorten: the
// startup retry and its pause, and the ping of the primary and its timeout.
type databaseTimings struct {
	retry, pause, ping, pingTimeout time.Duration
}

// defaultDatabaseTimings are the production intervals.
func defaultDatabaseTimings() databaseTimings {
	return databaseTimings{retry: startupRetry, pause: startupPause, ping: pingInterval, pingTimeout: pingTimeout}
}

// startWindow is the startup retry of one start: its end, which every read of
// the database at start shares, its length, which a failure names, and the
// pause between two attempts.
type startWindow struct {
	end          time.Time
	retry, pause time.Duration
}

// newStartWindow opens the startup retry of t, from now.
func newStartWindow(t databaseTimings) startWindow {
	return startWindow{end: time.Now().Add(t.retry), retry: t.retry, pause: t.pause}
}

// databaseHooks are what buildDatabase hands New besides the store and the
// leadership: logger, which names the primary's host, port and database, for
// the lines of New and of its runners about the database, and retry, which runs
// a read of New's at start within what is left of the start's startup retry,
// as the store's opening ran.
type databaseHooks struct {
	logger zerolog.Logger
	retry  func(ctx context.Context, attempt func(context.Context) error) error
}

// target is where a pool connects, as podium logs it: the host, the port and the
// database, never the user, the password or a parameter of the URL. Every line
// of the database mode names the primary's: its leadership's and its events
// transport's too, whose loggers carry it.
type target struct {
	host     string
	port     uint16
	database string
}

// logger is logger with where t connects, which every line of the database
// mode names in place of a URL.
func (t target) logger(logger zerolog.Logger) zerolog.Logger {
	return logger.With().Str("host", t.host).Uint16("port", t.port).Str("database", t.database).Logger()
}

// openPool returns a pool on the database that the URL of setting names, with
// application_name name and at most maxOpen connections, as many of them idle,
// and where it connects. It connects to nothing. The URL reaches the driver
// alone: one that does not parse fails with an error that names setting, never
// the URL, which carries a password. A URL that sets no connect_timeout, or 0,
// gets connectTimeout; one that sets another keeps it. TLS goes through the URL
// (sslmode, sslrootcert), whose files are read here.
func openPool(setting, rawURL, name string, maxOpen int) (*sql.DB, target, error) {
	cfg, err := poolConfig(setting, rawURL, name)
	if err != nil {
		return nil, target{}, err
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	return db, target{host: cfg.Host, port: cfg.Port, database: cfg.Database}, nil
}

// poolConfig is the driver's configuration of a pool of openPool: the URL of
// setting parsed, application_name name, and connectTimeout unless the URL sets
// a connect_timeout other than 0. Its error names setting, never the URL.
func poolConfig(setting, rawURL, name string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(rawURL)
	if err != nil {
		// pgx's error quotes the URL, its password redacted at best.
		return nil, fmt.Errorf("%s does not parse as a Postgres connection URL, or a file it names (sslrootcert, sslcert, sslkey) cannot be read", setting)
	}
	cfg.RuntimeParams["application_name"] = name
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = connectTimeout
	}
	return cfg, nil
}

// pools are the database mode's pools: the primary's, the replica's, nil without
// cluster.database.replica_url, and the lease's, of one connection, which the
// leadership needs apart from the store's, whose writers would starve it.
type pools struct {
	primary, replica, lease *sql.DB
	// where and replicaAt are where the primary's and the replica's connect.
	where, replicaAt target
}

// openPools opens the pools of cfg; none connects yet.
func openPools(cfg cluster.DatabaseConfig) (pools, error) {
	primary, where, err := openPool("cluster.database.url", cfg.URL, primaryName, cfg.OpenConns())
	if err != nil {
		return pools{}, err
	}
	lease, _, err := openPool("cluster.database.url", cfg.URL, leaseName, 1)
	if err != nil {
		_ = primary.Close()
		return pools{}, err
	}
	p := pools{primary: primary, lease: lease, where: where}
	if cfg.ReplicaURL != "" {
		replica, at, err := openPool("cluster.database.replica_url", cfg.ReplicaURL, replicaName, cfg.OpenConns())
		if err != nil {
			p.close()
			return pools{}, err
		}
		p.replica, p.replicaAt = replica, at
	}
	return p, nil
}

// close closes the pools.
func (p pools) close() {
	_ = p.lease.Close()
	if p.replica != nil {
		_ = p.replica.Close()
	}
	_ = p.primary.Close()
}

// buildDatabase builds the database mode of cfg, in this order: the pools; the
// leadership, with a random replica ID, which connects to nothing until it runs;
// within the startup retry (reach), the schema, migrated when cfg migrates, and
// the store on it, with the leadership as its fence; and the events transport,
// which becomes hub's writer before any event can be emitted. It then runs the
// leadership, the transport and the ping of the primary until the function it
// returns, which stops the ping, then the transport, then the leadership, whose
// end releases the lease, and closes the pools. The leader-only work under
// WhenLeader, the app's runners, has stopped before: App.Run waits for it, up to
// its drain deadline; work that outlived that deadline leaves the lease to
// expire (database.Leadership). o's metrics get the pools, the lease's counters
// and the dropped events, its probes the ping's failure, and its database hooks
// the logger and the rest of the startup retry.
func buildDatabase(cfg cluster.DatabaseConfig, logger zerolog.Logger, hub *event.Hub, o clusterOptions) (*sqlstore.Store, *database.Leadership, func(), error) {
	p, err := openPools(cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cluster: %w", err)
	}
	logger = p.where.logger(logger)
	lead, err := database.New(database.Options{
		DB:            p.primary,
		LeaseDB:       p.lease,
		ReplicaID:     rand.Text(),
		AdvertiseAddr: cfg.AdvertiseAddr,
		Lease:         cfg.Lease,
		Settings:      o.shared,
		Observer:      database.Observer{Renewed: o.metrics.LeaseRenewed, Lost: o.metrics.LeaseLost},
		Logger:        logger,
	})
	if err != nil {
		p.close()
		return nil, nil, nil, err
	}
	connecting := logger.Info().Str("replica_id", lead.ID()).Bool("migrate", cfg.Migrates())
	if p.replica != nil {
		connecting = connecting.Str("replica_host", p.replicaAt.host).Uint16("replica_port", p.replicaAt.port).Str("replica_database", p.replicaAt.database)
	}
	connecting.Msg("database: opening the store on the primary")

	var store *sqlstore.Store
	window := newStartWindow(o.database)
	err = reach(o.ctx, logger, p.where, window, func(ctx context.Context) error {
		if cfg.Migrates() {
			if err := migrateSchema(ctx, p.primary, logger); err != nil {
				return err
			}
		}
		s, err := sqlstore.Open(ctx, sqlstore.Options{Primary: p.primary, Replica: p.replica, Dialect: sqlstore.Postgres{}, Fence: lead})
		store = s
		return err
	})
	if err != nil {
		p.close()
		return nil, nil, nil, err
	}
	transport := database.NewEventTransport(p.primary, sqlstore.Postgres{}, eventsWaker(cfg.URL), hub, lead, logger)
	hub.SetWriter(transport)

	ping := &storePing{db: p.primary, every: o.database.ping, timeout: o.database.pingTimeout, logger: logger}
	stopLeadership := goRun(func(ctx context.Context) { _ = lead.Run(ctx) })
	stopTransport := goRun(func(ctx context.Context) { _ = transport.Run(ctx) })
	stopPing := goRun(ping.run)
	o.metrics.Database(p.primary, p.replica, p.lease, lead.AmLeader, transport.Dropped)
	if o.probes != nil {
		o.probes.failure, o.probes.elected = ping.failure, true
	}
	if o.dbHooks != nil {
		o.dbHooks.logger = logger
		o.dbHooks.retry = func(ctx context.Context, attempt func(context.Context) error) error {
			return reach(ctx, logger, p.where, window, attempt)
		}
	}
	return store, lead, func() {
		stopPing()
		stopTransport()
		stopLeadership()
		p.close()
	}, nil
}

// eventsWaker is the events listener the database mode builds on the primary's
// URL: a LISTEN on a connection of its own, application_name podium-events,
// whose errors name a server's SQLSTATE at most.
func eventsWaker(primaryURL string) database.Waker {
	return database.NewPostgresWaker(primaryURL, database.EventsChannel)
}

// goRun runs run in a goroutine of its own until the function it returns, which
// cancels run's context and waits for run to return. The context is none of a
// caller's: a shutdown signal reaches it through the app's Shutdown alone.
func goRun(run func(context.Context)) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx)
	}()
	return sync.OnceFunc(func() {
		cancel()
		<-done
	})
}

// reach runs attempt until it succeeds; until it fails with an error other than
// storage.ErrUnavailable, which ends the start at once, as a schema that is not
// this podium's, or a migration that fails, does; until ctx ends; or until w
// ends, each attempt bounded by that end, a pause of w.pause between two. So a
// start retries a database it cannot reach yet, every read of it at start
// within one window, and fails at its end, naming where it connects and the
// last failure, for main to exit 1. The first failure of a call logs one WARN
// on logger, which names where it connects.
func reach(ctx context.Context, logger zerolog.Logger, where target, w startWindow, attempt func(context.Context) error) error {
	deadline := w.end
	var last error
	for {
		attemptCtx, cancel := context.WithDeadline(ctx, deadline)
		err := attempt(attemptCtx)
		cancel()
		unavailable := errors.Is(err, storage.ErrUnavailable)
		// timedOut: the retry ended during the attempt.
		timedOut := errors.Is(err, context.DeadlineExceeded) && !time.Now().Before(deadline)
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case !unavailable && !timedOut:
			// Not a database it cannot reach: no later attempt fares otherwise.
			return err
		}
		if last == nil {
			logger.Warn().Err(err).Dur("startup_retry", w.retry).Msg(startRetrying)
		}
		if unavailable || last == nil {
			last = err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("database: the primary at %s port %d, database %s, did not answer within the startup retry of %s: %w", where.host, where.port, where.database, w.retry, last)
		}
		pause := time.NewTimer(min(w.pause, time.Until(deadline)))
		select {
		case <-ctx.Done():
			pause.Stop()
			return ctx.Err()
		case <-pause.C:
		}
	}
}

// migrateSchema brings the schema of db to this podium's, under the database
// lock (sqlstore.Migrate), and logs the versions before and after on logger,
// which names where db connects. A schema newer than this podium's fails it,
// both versions named, and nothing is written.
func migrateSchema(ctx context.Context, db *sql.DB, logger zerolog.Logger) error {
	before, err := sqlstore.SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if err := sqlstore.Migrate(ctx, db, sqlstore.Postgres{}); err != nil {
		return err
	}
	after, err := sqlstore.SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	done := logger.Info().Int("from_version", before).Int("to_version", after)
	if before == after {
		done.Msg("database: the schema is this podium's already")
		return nil
	}
	done.Msg("database: migrated the schema")
	return nil
}

// storePing pings the primary every interval, each ping within its timeout, and
// keeps the failure of the last one: while it failed, /readyz answers 503
// "storage failed", podium_store_failed is 1 and the gRPC health service
// NOT_SERVING. The first failure of a run logs a WARN, the success that ends the
// run an INFO, on a logger that names where the primary is. Before its first
// ping it reports no failure: New opened the store on the primary.
type storePing struct {
	db             *sql.DB
	every, timeout time.Duration
	logger         zerolog.Logger
	// last is the failure of the last ping, nil while it answered.
	last atomic.Pointer[error]
}

// failure returns the failure of the last ping, nil when it answered.
func (p *storePing) failure() error {
	if err := p.last.Load(); err != nil {
		return *err
	}
	return nil
}

// run pings the primary every interval until ctx ends.
func (p *storePing) run(ctx context.Context) {
	ticker := time.NewTicker(p.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		p.ping(ctx)
	}
}

// ping pings the primary once, and keeps what it answered. Its failure names
// the SQLSTATE of a server's refusal at most, never the URL.
func (p *storePing) ping(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	err := p.db.PingContext(pingCtx)
	switch {
	case ctx.Err() != nil:
		return
	case err == nil:
		if p.last.Swap(nil) != nil {
			p.logger.Info().Msg(pingRecovered)
		}
		return
	case pingCtx.Err() != nil:
		err = fmt.Errorf("%w: the primary did not answer a ping within %s", storage.ErrUnavailable, p.timeout)
	default:
		err = sqlstore.Postgres{}.Classify(err)
	}
	if p.last.Swap(&err) == nil {
		p.logger.Warn().Err(err).Msg(pingFailed)
	}
}

// dropWaiting is the WARN of podium drop-schema while replicas run on the
// database.
const dropWaiting = "database: replicas still run on the database: waiting for them to stop"

// DropSchema drops the schema of the database of cfg, as podium drop-schema
// does: on a pool of one connection to the primary, application_name
// podium-drop-schema, it drops every table of podium's, and the data in them,
// in one transaction under the lock on the migrations (sqlstore.DropSchema),
// once no replica runs on the database (database.CheckStopped). While replicas
// run it waits for them up to wait, logging one WARN, and then fails with
// database.ErrRunning, as it fails at once with no wait. Each attempt reaches the
// primary within the startup retry. It logs the host, the port, the database,
// the version before and the tables it dropped, or that there was no schema to
// drop. A schema newer than this podium's fails it at once, and nothing is
// dropped. It runs nothing else: no listener, no leadership, no advertise_addr.
// cfg is what config.LoadForMigration loaded.
func DropSchema(ctx context.Context, cfg config.Config, logger zerolog.Logger, wait time.Duration, opts ...Option) error {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	db, where, err := openPool("cluster.database.url", cfg.Cluster.Database.URL, dropSchemaName, 1)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	logger = where.logger(logger)
	logger.Info().Dur("wait", wait).Msg("database: dropping the schema")
	stopWaiting := time.Now().Add(wait)
	waited := false
	for {
		var (
			from    int
			dropped []string
		)
		err := reach(ctx, logger, where, newStartWindow(o.database), func(ctx context.Context) error {
			version, err := sqlstore.SchemaVersion(ctx, db)
			if err != nil {
				return err
			}
			from = version
			dropped, err = sqlstore.DropSchema(ctx, db, sqlstore.Postgres{}, database.CheckStopped)
			return err
		})
		switch {
		case err == nil && len(dropped) == 0:
			logger.Info().Msg("database: no schema to drop")
			return nil
		case err == nil:
			logger.Info().Int("from_version", from).Strs("tables", dropped).Msg("database: dropped the schema")
			return nil
		case !errors.Is(err, database.ErrRunning):
			return err
		case !time.Now().Before(stopWaiting):
			if waited {
				return fmt.Errorf("%w, after waiting %s for them to stop", err, wait)
			}
			return err
		}
		if !waited {
			logger.Warn().Err(err).Dur("wait", wait).Msg(dropWaiting)
			waited = true
		}
		pause := time.NewTimer(min(o.database.pause, time.Until(stopWaiting)))
		select {
		case <-ctx.Done():
			pause.Stop()
			return ctx.Err()
		case <-pause.C:
		}
	}
}

// Migrate brings the schema of the database of cfg to this podium's, as podium
// migrate does: on a pool of one connection to the primary, application_name
// podium-migrate, it migrates under the database lock within the startup retry,
// and logs the host, the port, the database and the schema versions before and
// after. A schema newer than this podium's fails it, both versions named, and so
// does any other failure, at once, or a primary it cannot reach within the
// retry. It runs nothing else: no listener, no leadership, no advertise_addr.
// cfg is what config.LoadForMigration loaded.
func Migrate(ctx context.Context, cfg config.Config, logger zerolog.Logger, opts ...Option) error {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	db, where, err := openPool("cluster.database.url", cfg.Cluster.Database.URL, migrateName, 1)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	logger = where.logger(logger)
	logger.Info().Msg("database: migrating the schema")
	return reach(ctx, logger, where, newStartWindow(o.database), func(ctx context.Context) error {
		return migrateSchema(ctx, db, logger)
	})
}
