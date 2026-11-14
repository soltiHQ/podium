package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// The tests of the database mode run whole apps, as main runs them, on a
// Postgres database of their own from pgtest, and skip without one; the second
// invocation of the Task gate ci/golang/test/postgres runs them. Their leases
// are short, and their intervals shortened through withDatabaseTimings.

// dbTimings are the database mode's intervals in the tests: a start retries the
// database for two seconds, and the primary is pinged every 100 ms.
var dbTimings = databaseTimings{retry: 2 * time.Second, pause: 50 * time.Millisecond, ping: 100 * time.Millisecond, pingTimeout: time.Second}

// withDatabaseTimings sets the database mode's intervals, which production
// never changes.
func withDatabaseTimings(d databaseTimings) Option {
	return func(o *options) { o.database = d }
}

const (
	// dbLease is the lease of the tests' replicas: a failover takes about a
	// second.
	dbLease = 1500 * time.Millisecond
	// dbAdminPassword is the administrator password of the tests' replicas,
	// SOLTI_BOOTSTRAP_ADMIN_PASSWORD.
	dbAdminPassword = "database-admin-password"
	// databaseForwardingWarning is the WARN of a replica of the database mode
	// that forwards in plaintext.
	databaseForwardingWarning = "database forwarding runs plaintext: without tls.server a follower forwards the writes it gets, logins, cookies and tokens included, and the agents' syncs to the leader over plain HTTP and gRPC; set tls.server on every node, and tls.client.ca_file to the CA of the nodes' server certificates"
)

// databaseNodes returns one replica of the database mode per host, none
// started, on the database at databaseURL: replica i serves its listeners on
// hosts[i], at ports that every replica shares when there are two or more, as
// the forwarding wants (the same-port assumption), or at ports the kernel picks
// for one, and advertises hosts[i]:0.
func databaseNodes(t *testing.T, databaseURL string, hosts ...string) []*clusterNode {
	t.Helper()
	ports := []string{"0", "0", "0", "0"}
	if len(hosts) > 1 {
		ports = sharedPorts(t, hosts, 4) // HTTP, HTTP discovery, gRPC and ops
	}
	nodes := make([]*clusterNode, 0, len(hosts))
	for i, host := range hosts {
		cfg := config.Default()
		cfg.Auth.JWTSecret = strings.Repeat("database-mode-test-secret", 2)
		cfg.Auth = cfg.Auth.WithDefaults()
		cfg.Cluster.Backend = cluster.BackendDatabase
		cfg.Cluster.Database.URL = databaseURL
		cfg.Cluster.Database.AdvertiseAddr = net.JoinHostPort(host, "0")
		cfg.Cluster.Database.Lease = dbLease
		cfg.BootstrapAdminPassword = dbAdminPassword
		cfg.HTTP.Addr = net.JoinHostPort(host, ports[0])
		cfg.HTTPDiscovery.Addr = net.JoinHostPort(host, ports[1])
		cfg.GRPC.Addr = net.JoinHostPort(host, ports[2])
		cfg.Ops.Addr = net.JoinHostPort(host, ports[3])
		nodes = append(nodes, &clusterNode{id: fmt.Sprintf("db%d", i+1), cfg: cfg, logs: &syncBuffer{}, opts: []Option{withDatabaseTimings(dbTimings)}})
	}
	return nodes
}

// databaseLeader waits until exactly one of nodes leads and every other knows
// it, and returns it and the others.
func databaseLeader(t *testing.T, nodes []*clusterNode) (leader *clusterNode, followers []*clusterNode) {
	t.Helper()
	waitFor(t, 10*time.Second, "one replica leading, known to the others", func() bool {
		leader, followers = nil, nil
		for _, n := range nodes {
			if n.app.leadership.AmLeader() {
				if leader != nil {
					return false
				}
				leader = n
				continue
			}
			followers = append(followers, n)
		}
		if leader == nil {
			return false
		}
		for _, f := range followers {
			if f.app.leadership.CurrentLeader() != leader.cfg.Cluster.Database.AdvertiseAddr {
				return false
			}
		}
		return true
	})
	return leader, followers
}

// adminSeeded reports whether store holds the administrator with password.
func adminSeeded(store storage.Storage, password string) bool {
	ctx := context.Background()
	c, err := store.GetCredentialByUserAndAuth(ctx, "user-admin", enum.Password)
	if err != nil {
		return false
	}
	v, err := store.GetVerifierByCredential(ctx, c.ID())
	return err == nil && credentials.VerifyPassword(c, v, password) == nil
}

// serverDB is a pool on the server of the tests, on its own database, whose
// user is a superuser: what a test does to the server from outside podium.
func serverDB(t *testing.T) *sql.DB {
	t.Helper()
	return pgtest.Open(t, os.Getenv(pgtest.EnvURL))
}

// databaseName is the name of the database at databaseURL.
func databaseName(t *testing.T, databaseURL string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal("the database URL does not parse")
	}
	return strings.TrimPrefix(u.Path, "/")
}

// exec runs statement with args on db, and fails the test when it fails.
func exec(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// endSessionsUntil ends, from server, every session that where, a condition on
// pg_stat_activity with args, selects, round after round, until done holds. A
// connection that was setting up while the test changed the server completes
// with what it read before the change, a password or the database's
// datallowconn, and is not listed until it has: a later round ends it, so that
// only connections that meet the change are left.
func endSessionsUntil(t *testing.T, server *sql.DB, what string, done func() bool, where string, args ...any) {
	t.Helper()
	waitFor(t, 10*time.Second, what, func() bool {
		exec(t, server, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE `+where, args...)
		return done()
	})
}

// storageFailed reports whether the ops listener at addr answers /readyz with
// 503 "storage failed".
func storageFailed(addr string) bool {
	status, body, err := get(addr, "/readyz", nil)
	return err == nil && status == http.StatusServiceUnavailable && strings.TrimSpace(body) == reasonStorage
}

// urlSecrets are the forms in which the credentials of databaseURL would show
// if a log line, an error or a page carried them: the URL itself, the scheme
// that starts one, the user and password as a URL holds them, and as a
// connection string or a driver's message names them. Bare words are left out:
// the gate's user is postgres, and its password is part of its host's name.
func urlSecrets(t *testing.T, databaseURL string) []string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal("the database URL does not parse")
	}
	secrets := []string{databaseURL, "postgres://", "postgresql://"}
	if u.User != nil {
		user := u.User.Username()
		secrets = append(secrets, "user="+user, `user "`+user+`"`)
		if password, ok := u.User.Password(); ok {
			secrets = append(secrets, ":"+password+"@", "password="+password)
		}
	}
	return secrets
}

// noSecrets fails the test when any of texts holds any of secrets, naming the
// text, never the secret.
func noSecrets(t *testing.T, secrets []string, texts map[string]string) {
	t.Helper()
	for what, text := range texts {
		for i, secret := range secrets {
			if secret != "" && strings.Contains(text, secret) {
				t.Errorf("%s holds secret form %d of the database URL", what, i)
			}
		}
	}
}

// An app on Postgres serves: it migrates the empty database, takes the lease
// and seeds the administrator with SOLTI_BOOTSTRAP_ADMIN_PASSWORD, /readyz and
// /leaderz answer 200, a user created through the API is read back through it,
// the listing through the replica's pool, and the metrics show the leader, the
// three pools and a store that serves. Each pool and the events listener name
// themselves in pg_stat_activity. No log line, metric or /version carries the
// URL or its credentials.
func TestDatabaseModeServes(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	n := databaseNodes(t, databaseURL, "127.0.0.1")[0]
	// The database is its own read replica: the listings take the replica's pool.
	n.cfg.Cluster.Database.ReplicaURL = databaseURL
	n.start(t)
	ops := n.app.ops.Addr
	waitFor(t, 10*time.Second, "the listeners bind", func() bool { return ops() != "" && n.app.ui.Addr() != "" })
	opsAnswers(t, ops(), "/readyz", 200, "ok")
	opsAnswers(t, ops(), "/leaderz", 200, "ok")
	waitFor(t, 10*time.Second, "the leader seeding the administrator", func() bool { return adminSeeded(n.app.store, dbAdminPassword) })
	// The seed writes through the store: its lines name the primary and the
	// replica that seeded. It logs its line once the commit that adminSeeded saw
	// returned.
	var seeded []logLine
	waitFor(t, 10*time.Second, "the line of the seed", func() bool {
		seeded = n.logged(t, map[string]any{"message": "bootstrap: admin password set from " + bootstrap.AdminPasswordEnv})
		return len(seeded) > 0
	})
	if len(seeded) != 1 {
		t.Fatalf("the line of the seed logged %d times, want once", len(seeded))
	}
	if f := seeded[0].fields; f["database"] != databaseName(t, databaseURL) || f["host"] == nil || f["port"] == nil || f["replica_id"] != n.app.leadership.(interface{ ID() string }).ID() {
		t.Errorf("the seed logged %v, want the primary's host, port and database and the replica's ID", f)
	}

	token := bearerFor(t, n.app, n.cfg, "operator", enum.UsersGet, enum.UsersAdd)
	ui := "http://" + n.app.ui.Addr()
	if resp, body := send(t, httpClient(), http.MethodPost, ui+"/api/v1/users", jsonType, `{"subject":"carol"}`, token, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /api/v1/users: %d %q, want 204", resp.StatusCode, body)
	}
	resp, body := send(t, httpClient(), http.MethodGet, ui+"/api/v1/users", "", "", token, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"carol"`) {
		t.Fatalf("GET /api/v1/users: %d %q, want carol listed", resp.StatusCode, body)
	}
	if u, err := n.app.store.GetUserBySubject(context.Background(), "carol"); err != nil || u.Subject() != "carol" {
		t.Fatalf("carol read back from the store: %v, %v", u, err)
	}

	metrics := scrapeOps(t, ops())
	for _, want := range []string{
		"podium_store_failed 0",
		"podium_database_leader 1",
		`podium_database_pool_max_open_connections{pool="primary"} 10`,
		`podium_database_pool_max_open_connections{pool="replica"} 10`,
		`podium_database_pool_max_open_connections{pool="lease"} 1`,
		"podium_database_events_dropped_total 0",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("the exposition lacks %q", want)
		}
	}
	if v, ok := series(metrics, "podium_database_lease_renewals_total"); !ok {
		t.Errorf("podium_database_lease_renewals_total: %v, %v; want a series", v, ok)
	}

	// Each pool, and the events listener, names itself to the server.
	var names []string
	rows, err := serverDB(t).QueryContext(context.Background(), `SELECT DISTINCT application_name FROM pg_stat_activity WHERE datname = $1`, databaseName(t, databaseURL))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	_ = rows.Close()
	for _, want := range []string{"podium", "podium-replica", "podium-lease", "podium-events"} {
		if !slices.Contains(names, want) {
			t.Errorf("no session of %s on the database, which has %q", want, names)
		}
	}

	_, version, err := get(ops(), "/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	noSecrets(t, urlSecrets(t, databaseURL), map[string]string{"the log": n.logs.String(), "the metrics": metrics, "/version": version})
}

// Two apps on one database, each on a loopback IP of its own with the same
// ports: one leads, and a write sent to the other, the creation of a user,
// lands through the leader, whose access log serves it; the follower counts the
// forward, and its UI sees the change event, which the database carried: the
// record of the creation in its hub, and the refresh pulse to its streams. Both
// replicas, which run without tls.server, warn at start of their plaintext
// forwarding.
func TestTwoReplicasForwardWrites(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	nodes := databaseNodes(t, databaseURL, loopbackIPs(t, 2)...)
	for _, n := range nodes {
		n.start(t)
	}
	serving(t, nodes...)
	leader, followers := databaseLeader(t, nodes)
	follower := followers[0]
	for _, n := range nodes {
		if got := n.logged(t, map[string]any{"level": "warn", "message": databaseForwardingWarning}); len(got) != 1 {
			t.Fatalf("%s logged the WARN of plaintext forwarding %d times, want once", n.id, len(got))
		}
	}

	pulses := follower.app.eventHub.Subscribe(t.Context())
	token := bearerFor(t, leader.app, leader.cfg, "operator", enum.UsersGet, enum.UsersAdd)
	resp, body := send(t, httpClient(), http.MethodPost, "http://"+follower.cfg.HTTP.Addr+"/api/v1/users", jsonType, `{"subject":"dave"}`, token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /api/v1/users through %s: %d %q, want the leader's 204", follower.id, resp.StatusCode, body)
	}
	waitFor(t, 5*time.Second, leader.id+" logs the creation", func() bool {
		return servedAPI(t, leader, http.MethodPost, "/api/v1/users", http.StatusNoContent)
	})
	if servedAPI(t, follower, http.MethodPost, "/api/v1/users", http.StatusNoContent) {
		t.Fatalf("%s served the write itself", follower.id)
	}
	dave, err := follower.app.store.GetUserBySubject(context.Background(), "dave")
	if err != nil {
		t.Fatalf("dave read back on %s: %v", follower.id, err)
	}
	if v, ok := series(scrapeOps(t, follower.cfg.Ops.Addr), `podium_forwarded_writes_total{server="ui"}`); !ok || v != 1 {
		t.Errorf("%s's forwarded UI writes: %v, %v; want 1", follower.id, v, ok)
	}

	waitFor(t, 10*time.Second, follower.id+"'s hub holding the creation", func() bool {
		for _, rec := range follower.app.eventHub.RecentEvents(event.RingSize) {
			if rec.Kind == event.UserCreated && rec.Payload.ID == dave.ID() {
				return true
			}
		}
		return false
	})
	deadline := time.After(10 * time.Second)
	for refreshed := false; !refreshed; {
		select {
		case pulse := <-pulses:
			refreshed = pulse == event.RefreshUsers
		case <-deadline:
			t.Fatalf("%s's streams got no %s pulse within 10s", follower.id, event.RefreshUsers)
		}
	}
}

// An empty database and no SOLTI_BOOTSTRAP_ADMIN_PASSWORD: the start stops with
// ErrAdminPasswordRequired, naming the variable, and seeds nothing. Once a start
// with the variable seeded the administrator, a start without it serves.
func TestBootstrapNeedsPassword(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	n := databaseNodes(t, databaseURL, "127.0.0.1")[0]
	n.cfg.BootstrapAdminPassword = ""
	err := n.tryStart(t)
	if !errors.Is(err, bootstrap.ErrAdminPasswordRequired) || !strings.Contains(err.Error(), bootstrap.AdminPasswordEnv) {
		t.Fatalf("a start on an empty database without the password: %v, want ErrAdminPasswordRequired naming %s", err, bootstrap.AdminPasswordEnv)
	}
	var users int
	if err := pgtest.Open(t, databaseURL).QueryRowContext(context.Background(), `SELECT count(*) FROM podium_users`).Scan(&users); err != nil || users != 0 {
		t.Fatalf("users after the refused start: %d, %v; want none", users, err)
	}

	seeding := databaseNodes(t, databaseURL, "127.0.0.1")[0]
	seeding.start(t)
	waitFor(t, 10*time.Second, "the administrator seeded", func() bool { return adminSeeded(seeding.app.store, dbAdminPassword) })
	seeding.stop()

	again := databaseNodes(t, databaseURL, "127.0.0.1")[0]
	again.cfg.BootstrapAdminPassword = ""
	again.start(t)
	waitFor(t, 10*time.Second, "the start without the password leading", again.app.leadership.AmLeader)
}

// The database goes away under a running app, as at a restart or a failover:
// it takes no connection, and a superuser ends every session of podium, each
// pool's and the events listener's, which name themselves. Requests answer 503,
// /readyz says "storage failed", podium_store_failed is 1, the gRPC health
// service NOT_SERVING, and the leader steps down. Once the database takes
// connections again, all of it recovers without a restart: reads and writes
// answer, /readyz is ready, and the lease is taken again. No log line carries
// the URL or its credentials.
func TestDatabaseOutageRecovers(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	name := databaseName(t, databaseURL)
	server := serverDB(t)
	n := databaseNodes(t, databaseURL, "127.0.0.1")[0]
	n.start(t)
	ops := n.app.ops.Addr
	waitFor(t, 10*time.Second, "the listeners bind", func() bool { return ops() != "" && n.app.ui.Addr() != "" })
	opsAnswers(t, ops(), "/readyz", 200, "ok")
	waitFor(t, 10*time.Second, "the replica leading", n.app.leadership.AmLeader)
	token := bearerFor(t, n.app, n.cfg, "operator", enum.UsersGet, enum.UsersAdd)
	ui := "http://" + n.app.ui.Addr()
	answers := func(method, path, body string) int {
		resp, _ := send(t, httpClient(), method, ui+path, jsonType, body, token, nil)
		return resp.StatusCode
	}
	health := func() healthpb.HealthCheckResponse_ServingStatus {
		resp, err := n.app.grpcHealth.Check(context.Background(), &healthpb.HealthCheckRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetStatus()
	}
	if got := answers(http.MethodGet, "/api/v1/users", ""); got != http.StatusOK {
		t.Fatalf("GET /api/v1/users before the outage: %d, want 200", got)
	}

	exec(t, server, `ALTER DATABASE `+name+` ALLOW_CONNECTIONS false`)
	t.Cleanup(func() {
		_, _ = server.ExecContext(context.Background(), `ALTER DATABASE `+name+` ALLOW_CONNECTIONS true`)
	})
	// Until /readyz says so and the leader, whose renewals stop landing, stepped down.
	endSessionsUntil(t, server, "storage failed and the leader stepping down once podium's sessions ended", func() bool {
		return storageFailed(ops()) && !n.app.leadership.AmLeader()
	}, `datname = $1 AND application_name IN ('podium', 'podium-replica', 'podium-lease', 'podium-events')`, name)

	waitFor(t, 10*time.Second, "a read answering 503", func() bool { return answers(http.MethodGet, "/api/v1/users", "") == http.StatusServiceUnavailable })
	if v, ok := series(scrapeOps(t, ops()), "podium_store_failed"); !ok || v != 1 {
		t.Errorf("podium_store_failed during the outage: %v, %v; want 1", v, ok)
	}
	if got := health(); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Errorf("gRPC health during the outage: %v, want NOT_SERVING", got)
	}
	if got := answers(http.MethodPost, "/api/v1/users", `{"subject":"lost"}`); got != http.StatusServiceUnavailable {
		t.Errorf("a write during the outage: %d, want 503", got)
	}

	exec(t, server, `ALTER DATABASE `+name+` ALLOW_CONNECTIONS true`)
	opsAnswers(t, ops(), "/readyz", 200, "ok")
	waitFor(t, 10*time.Second, "a read answering 200", func() bool { return answers(http.MethodGet, "/api/v1/users", "") == http.StatusOK })
	waitFor(t, 10*time.Second, "the replica leading again", n.app.leadership.AmLeader)
	if got := answers(http.MethodPost, "/api/v1/users", `{"subject":"erin"}`); got != http.StatusNoContent {
		t.Errorf("a write once the database is back: %d, want 204", got)
	}
	metrics := scrapeOps(t, ops())
	if v, ok := series(metrics, "podium_store_failed"); !ok || v != 0 {
		t.Errorf("podium_store_failed once the database is back: %v, %v; want 0", v, ok)
	}
	if v, ok := series(metrics, "podium_database_lease_losses_total"); !ok || v < 1 {
		t.Errorf("podium_database_lease_losses_total after the outage: %v, %v; want 1 at least", v, ok)
	}
	if got := health(); got != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("gRPC health once the database is back: %v, want SERVING", got)
	}
	// Each run of failed pings logs one WARN, and the ping that ends it one INFO.
	failedRuns := len(n.logged(t, map[string]any{"level": "warn", "message": pingFailed}))
	if ended := len(n.logged(t, map[string]any{"level": "info", "message": pingRecovered})); failedRuns < 1 || failedRuns != ended {
		t.Errorf("the WARN of a failed ping logged %d times and the INFO of its end %d times, want the same, once at least", failedRuns, ended)
	}
	noSecrets(t, urlSecrets(t, databaseURL), map[string]string{"the log": n.logs.String()})
}

// A start retries a database it cannot reach for the startup retry, and serves
// once the database takes connections within it; it logs one WARN for the
// failures, naming where it connects, never the URL.
func TestDatabaseStartRetries(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	name := databaseName(t, databaseURL)
	server := serverDB(t)
	exec(t, server, `ALTER DATABASE `+name+` ALLOW_CONNECTIONS false`)
	t.Cleanup(func() {
		_, _ = server.ExecContext(context.Background(), `ALTER DATABASE `+name+` ALLOW_CONNECTIONS true`)
	})
	n := databaseNodes(t, databaseURL, "127.0.0.1")[0]
	timings := dbTimings
	timings.retry = 20 * time.Second
	built := make(chan error, 1)
	var a *App
	go func() {
		var err error
		a, err = New(context.Background(), n.cfg, zerolog.New(n.logs), WithPasswordCost(bcrypt.MinCost), withDatabaseTimings(timings))
		built <- err
	}()
	waitFor(t, 10*time.Second, "the WARN of a start that retries", func() bool {
		return strings.Contains(n.logs.String(), startRetrying)
	})
	exec(t, server, `ALTER DATABASE `+name+` ALLOW_CONNECTIONS true`)
	if err := receiveErr(t, built, 20*time.Second); err != nil {
		t.Fatalf("New once the database takes connections: %v", err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := n.logged(t, map[string]any{"level": "warn", "message": startRetrying})
	if len(lines) != 1 {
		t.Fatalf("the WARN of a start that retries logged %d times, want once", len(lines))
	}
	if lines[0].fields["database"] != name || lines[0].fields["host"] == nil || lines[0].fields["port"] == nil {
		t.Errorf("the WARN names %v, want the host, the port and the database %s", lines[0].fields, name)
	}
	// The database refused the connection as it took none: 55000, its SQLSTATE
	// and nothing else of the server's message.
	if text, _ := lines[0].fields["error"].(string); !strings.Contains(text, "SQLSTATE 55000") || strings.Contains(text, "not currently accepting") {
		t.Errorf("the WARN's error is %q, want the SQLSTATE 55000 of a database that takes no connection, and no message", text)
	}
	noSecrets(t, urlSecrets(t, databaseURL), map[string]string{"the log": n.logs.String()})
}

// receiveErr returns what ch sends within the time given, and fails the test
// otherwise.
func receiveErr(t *testing.T, ch <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(within):
		t.Fatalf("nothing within %s", within)
		return nil
	}
}

// leak are the distinctive credentials of TestDatabaseURLNeverLeaks, which no
// log line or error may carry.
const (
	leakUser     = "podium_leak_user"
	leakPassword = "leak-pw-7f3a9c"
)

// leakSecrets are what TestDatabaseURLNeverLeaks looks for: its distinctive
// user and password, the URLs it hands podium, and the scheme of a URL.
func leakSecrets(urls ...string) []string {
	return append([]string{leakUser, leakPassword, url.QueryEscape(leakPassword), "postgres://", "postgresql://"}, urls...)
}

// withCredentials is databaseURL with user and password.
func withCredentials(t *testing.T, databaseURL, user, password string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal("the database URL does not parse")
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// closedPort is a TCP address on the loopback that nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// No log line or error carries a database URL, its user or its password, each
// distinctive, whatever fails: a wrong password, a host that does not answer, a
// URL that does not parse, of the primary, which fails the start, of the read
// replica, whose listings then fall back to the primary, and of the events
// listener, which the app builds on the primary's URL. Through the app the
// listener only meets a wrong password once the primary's password changes
// under it (the primary's URL parsed and answered before the listener starts),
// so that case runs whole: the role's password changes and its sessions end,
// and every pool and the listener fail to reconnect. The listener's own errors
// are also checked for all three cases, as the app builds it.
func TestDatabaseURLNeverLeaks(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	wrongPassword := withCredentials(t, databaseURL, leakUser, leakPassword)
	unreachable := "postgres://" + leakUser + ":" + leakPassword + "@" + closedPort(t) + "/podium?sslmode=disable"
	malformed := "postgres://" + leakUser + ":" + leakPassword + "@db.podium.test:notaport/podium"
	short := dbTimings
	short.retry = 300 * time.Millisecond

	for _, tc := range []struct{ name, url string }{
		{"wrong password", wrongPassword},
		{"unreachable host", unreachable},
		{"malformed URL", malformed},
	} {
		t.Run("primary/"+tc.name, func(t *testing.T) {
			n := databaseNodes(t, tc.url, "127.0.0.1")[0]
			n.opts = []Option{withDatabaseTimings(short)}
			err := n.tryStart(t)
			if err == nil {
				t.Fatal("the start succeeded")
			}
			// A failure that reaches the database is retried until the startup
			// retry ends; a URL that does not parse fails at once.
			want := "did not answer within the startup retry"
			if tc.url == malformed {
				want = "cluster.database.url does not parse"
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the start failed with %q, want it to say %q", err, want)
			}
			noSecrets(t, leakSecrets(tc.url), map[string]string{"the error": err.Error(), "the log": n.logs.String()})
		})

		t.Run("replica/"+tc.name, func(t *testing.T) {
			n := databaseNodes(t, databaseURL, "127.0.0.1")[0]
			n.cfg.Cluster.Database.ReplicaURL = tc.url
			if tc.url == malformed {
				err := n.tryStart(t)
				if err == nil || !strings.Contains(err.Error(), "cluster.database.replica_url") {
					t.Fatalf("a start with a malformed replica URL: %v, want an error naming cluster.database.replica_url", err)
				}
				noSecrets(t, leakSecrets(tc.url), map[string]string{"the error": err.Error(), "the log": n.logs.String()})
				return
			}
			n.start(t)
			waitFor(t, 10*time.Second, "the replica leading", n.app.leadership.AmLeader)
			// The subtests share the database: each reader is a user of its own.
			token := bearerFor(t, n.app, n.cfg, "reader-"+strings.ReplaceAll(tc.name, " ", "-"), enum.UsersGet)
			resp, body := send(t, httpClient(), http.MethodGet, "http://"+n.app.ui.Addr()+"/api/v1/users", "", "", token, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("a listing with the replica failing: %d, want the primary's 200", resp.StatusCode)
			}
			noSecrets(t, leakSecrets(tc.url), map[string]string{"the answer": body, "the log": n.logs.String(), "the metrics": scrapeOps(t, n.app.ops.Addr())})
		})

		t.Run("listener/"+tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err := eventsWaker(tc.url).Wait(ctx)
			if err == nil {
				t.Fatal("the listener connected")
			}
			noSecrets(t, leakSecrets(tc.url), map[string]string{"the error": err.Error()})
		})
	}

	t.Run("password changed under a running app", func(t *testing.T) {
		server := serverDB(t)
		role := fmt.Sprintf("%s_%x", leakUser, rand.Text()[:6])
		password := leakPassword + "-" + strings.ToLower(rand.Text()[:6])
		exec(t, server, `CREATE ROLE `+role+` LOGIN PASSWORD '`+password+`'`)
		// The role goes once its database has: its cleanup runs after pgtest's.
		t.Cleanup(func() { _, _ = server.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+role) })
		own := pgtest.NewDatabase(t)
		exec(t, server, `ALTER DATABASE `+databaseName(t, own)+` OWNER TO `+role)
		roleURL := withCredentials(t, own, role, password)

		n := databaseNodes(t, roleURL, "127.0.0.1")[0]
		n.start(t)
		ops := n.app.ops.Addr
		waitFor(t, 10*time.Second, "the ops listener binding", func() bool { return ops() != "" })
		opsAnswers(t, ops(), "/readyz", 200, "ok")
		waitFor(t, 10*time.Second, "the replica leading", n.app.leadership.AmLeader)

		exec(t, server, `ALTER ROLE `+role+` PASSWORD 'another-`+password+`'`)
		endSessionsUntil(t, server, "storage failed and the leader stepping down once the role's sessions ended", func() bool {
			return storageFailed(ops()) && !n.app.leadership.AmLeader()
		}, `usename = $1`, role)
		waitFor(t, 10*time.Second, "the events listener failing to reconnect", func() bool {
			return strings.Contains(n.logs.String(), "database: the events listener failed")
		})
		noSecrets(t, append(leakSecrets(roleURL), role, password), map[string]string{"the log": n.logs.String(), "the metrics": scrapeOps(t, ops())})
		if !strings.Contains(n.logs.String(), "28P01") {
			t.Error("no log line names the server's refusal, SQLSTATE 28P01")
		}
	})
}

// The schema of a database migrated by a newer podium stops the start with the
// versions named, and nothing writes to it; with migrate false an older one
// stops it too, saying to run podium migrate.
func TestDatabaseSchemaNotOursStopsTheStart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int // recorded before the start; 0 leaves the database empty
		migrate bool
		want    error
		says    string
	}{
		{name: "newer", version: 99, migrate: true, want: sqlstore.ErrSchemaNewer, says: "version 99"},
		{name: "older without migrations", migrate: false, want: sqlstore.ErrSchemaOlder, says: "run podium migrate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			databaseURL := pgtest.NewDatabase(t)
			db := pgtest.Open(t, databaseURL)
			if tc.version > 0 {
				exec(t, db, `CREATE TABLE podium_schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`)
				exec(t, db, `INSERT INTO podium_schema_migrations (version) VALUES ($1)`, tc.version)
			}
			n := databaseNodes(t, databaseURL, "127.0.0.1")[0]
			n.cfg.Cluster.Database.Migrate = &tc.migrate
			begin := time.Now()
			err := n.tryStart(t)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("the start: %v, want %v saying %q", err, tc.want, tc.says)
			}
			if took := time.Since(begin); took >= dbTimings.retry {
				t.Errorf("the start took %s to stop, want at once: a schema that is not this podium's is not retried", took)
			}
			if v, err := sqlstore.SchemaVersion(context.Background(), db); err != nil || v != tc.version {
				t.Errorf("the schema after the refusal: %d, %v; want %d, untouched", v, err, tc.version)
			}
		})
	}
}

// Replicas of the database mode check the settings every replica must share
// against their leader's, from the registry: a replica whose JWT secret
// differs from the leader's logs the ERROR of its configuration check, naming
// the group auth and the leader's replica ID, as on raft; one that shares the
// leader's settings finds them matching, and the leader checks nothing.
func TestDatabaseReplicaSettingsChecked(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	// No write is forwarded: each replica takes ports of its own.
	var nodes []*clusterNode
	for i := range 3 {
		n := databaseNodes(t, databaseURL, "127.0.0.1")[0]
		n.id = fmt.Sprintf("db%d", i+1)
		nodes = append(nodes, n)
	}
	drifted := nodes[2]
	drifted.cfg.Auth.JWTSecret = strings.Repeat("drifted-database-replica-secret", 2)
	for _, n := range nodes[:2] {
		n.start(t)
	}
	leader, followers := databaseLeader(t, nodes[:2])
	drifted.start(t)
	leaderID := leader.app.leadership.(interface{ ID() string }).ID()

	waitFor(t, 10*time.Second, drifted.id+" logging the ERROR of its configuration check", func() bool {
		return len(drifted.logged(t, map[string]any{"runner": configCheck, "level": "error"})) > 0
	})
	name := databaseName(t, databaseURL)
	for _, line := range drifted.logged(t, map[string]any{"runner": configCheck, "level": "error"}) {
		groups, _ := line.fields["groups"].([]any)
		if line.fields["message"] != configDiffers || len(groups) != 1 || groups[0] != cluster.GroupAuth || line.fields["leader_id"] != leaderID {
			t.Fatalf("%s logged %v, want %q naming the group auth and the leader %s", drifted.id, line.fields, configDiffers, leaderID)
		}
		// The check reads the registry: its lines name the primary, as the
		// database mode's do.
		if line.fields["database"] != name || line.fields["host"] == nil || line.fields["port"] == nil {
			t.Fatalf("%s's check logged %v, want the primary's host, port and database %s", drifted.id, line.fields, name)
		}
	}
	waitFor(t, 10*time.Second, followers[0].id+" finding its leader's settings matching", func() bool {
		return len(followers[0].logged(t, map[string]any{"runner": configCheck, "message": configMatches, "leader_id": leaderID})) > 0
	})
	if lines := leader.logged(t, map[string]any{"runner": configCheck, "level": "error"}); len(lines) != 0 {
		t.Fatalf("the leader %s logged %v: a replica that leads checks nothing", leader.id, lines)
	}
	if strings.Contains(drifted.logs.String(), drifted.cfg.Auth.JWTSecret) || strings.Contains(drifted.logs.String(), leader.cfg.Auth.JWTSecret) {
		t.Fatal("a log line holds a JWT secret")
	}
}

// The read of the user table that decides whether a start needs
// SOLTI_BOOTSTRAP_ADMIN_PASSWORD takes the rest of the startup retry, as the
// store's opening does: a test holds the table locked, ends the session whose
// read waits for the lock, which fails it as a lost connection (57P01), and
// lets the table go; the start retries the read and serves. The WARN of the
// retry names the SQLSTATE.
func TestDatabaseStartRetriesTheSeedCheck(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	seeding := databaseNodes(t, databaseURL, "127.0.0.1")[0]
	seeding.start(t)
	waitFor(t, 10*time.Second, "the administrator seeded", func() bool { return adminSeeded(seeding.app.store, dbAdminPassword) })
	seeding.stop()

	name := databaseName(t, databaseURL)
	server := serverDB(t)
	admin := pgtest.Open(t, databaseURL)
	lock, err := admin.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Rollback() })
	if _, err := lock.ExecContext(context.Background(), `LOCK TABLE podium_users IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	// Without the variable, the start reads the user table to know it needs none.
	n := databaseNodes(t, databaseURL, "127.0.0.1")[0]
	n.cfg.BootstrapAdminPassword = ""
	timings := dbTimings
	timings.retry = 20 * time.Second
	built := make(chan error, 1)
	var a *App
	go func() {
		var err error
		a, err = New(context.Background(), n.cfg, zerolog.New(n.logs), WithPasswordCost(bcrypt.MinCost), withDatabaseTimings(timings))
		built <- err
	}()
	var waiting int
	waitFor(t, 10*time.Second, "the start's read of the user table waiting for the lock", func() bool {
		err := server.QueryRowContext(context.Background(), `SELECT count(*) FROM pg_stat_activity
 WHERE datname = $1 AND application_name = 'podium' AND wait_event_type = 'Lock'`, name).Scan(&waiting)
		return err == nil && waiting > 0
	})
	exec(t, server, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE datname = $1 AND application_name = 'podium' AND wait_event_type = 'Lock'`, name)
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := receiveErr(t, built, 20*time.Second); err != nil {
		t.Fatalf("New once its read was ended and the table let go: %v", err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := n.logged(t, map[string]any{"level": "warn", "message": startRetrying})
	if len(lines) != 1 {
		t.Fatalf("the WARN of a start that retries logged %d times, want once, for the read of the user table", len(lines))
	}
	if text, _ := lines[0].fields["error"].(string); !strings.Contains(text, "SQLSTATE 57P01") {
		t.Errorf("the WARN's error is %q, want the SQLSTATE 57P01 of the ended session", text)
	}
}
