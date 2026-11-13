// Package raft wires hashicorp/raft on top of the inmemory store.
//
// Usage:
//
//	profile, err := raft.New(ctx, raft.Config{...}, inmemory.New(), disco, logger)
//	store := profile.Store()                 // storage.Storage wired through raft
//	leadership := profile.Leadership()       // cluster.Leadership
//
// Transport: TCP with mutual TLS, or plain TCP when explicitly allowed.
// Log store: BoltDB on disk (so logs survive restart), in a data directory
// only its owner can enter, as are the snapshots.
// Peer bootstrap: from [cluster.Discovery] — on first start every node
// bootstraps the cluster with the same list, every peer Discovery reports
// sorted by ID, which must name this node; a node that joins a running cluster
// bootstraps nothing, and the leader adds it ([Admin]). Subsequent starts join a
// pre-existing cluster and reuse the on-disk log.
// Logging: hashicorp/raft, its snapshot store and its transport log through the
// logger given to New, at their own levels from INFO, and write nothing to stderr.
package raft

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/rs/zerolog"
	bolt "go.etcd.io/bbolt"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Config parametrises a Profile.
type Config struct {
	// NodeID is the Raft server identifier for this replica. Must be
	// unique across the cluster and stable across restarts.
	NodeID string

	// BindAddr is the TCP address the Raft transport listens on (host:port).
	// Other replicas reach this replica at BindAddr.
	BindAddr string

	// AdvertiseAddr overrides BindAddr for peer-facing addressing (use
	// when bind is 0.0.0.0 but peers must see an explicit host). If empty,
	// BindAddr is used. Discovery must list this node at this address, as
	// written.
	AdvertiseAddr string

	// DataDir holds the BoltDB log store and snapshots. Required.
	DataDir string

	// ElectionTimeout / HeartbeatTimeout tune the Raft timings. Zero
	// values use hashicorp/raft defaults (1s / 1s). For in-DC deployments
	// you can lower to 100-200ms; for cross-DC bump to 3-5s.
	ElectionTimeout  time.Duration
	HeartbeatTimeout time.Duration

	// SnapshotInterval is how often the node checks whether to snapshot,
	// SnapshotThreshold the number of log entries since the last snapshot that
	// starts one, and TrailingLogs the number of entries a snapshot leaves in
	// the log, for a slow follower to catch up from. Zero values use
	// hashicorp/raft's defaults (120 s, 8192 and 10240). hashicorp/raft refuses
	// an interval under 5 ms when New starts it, which
	// [cluster.Config.Validate] refuses earlier.
	SnapshotInterval  time.Duration
	SnapshotThreshold uint64
	TrailingLogs      uint64

	// TLS names the PEM files of mutual TLS between nodes, see
	// [cluster.RaftTLSConfig]. A node without them runs its transport on plain
	// TCP, which New refuses unless AllowInsecureTransport is set.
	TLS cluster.RaftTLSConfig

	// AllowInsecureTransport permits plain TCP when TLS names no file. New
	// logs a WARN when the transport runs it.
	AllowInsecureTransport bool

	// Join starts a node whose DataDir holds no raft state without
	// bootstrapping: it neither asks the discovery nor writes a configuration,
	// and waits for the leader of a running cluster to add it. A node with
	// raft state ignores it.
	Join bool

	// ConfigDigests are the digests of the settings that every replica must
	// share ([cluster.SharedSettings]). The node answers a peer's configuration
	// request on its raft port with them, and its configuration check
	// ([Admin.ConfigCheck]) compares them with its leader's. A node without
	// them answers no such request, as a version without the check does, and
	// has no check.
	ConfigDigests *cluster.Digests
}

// Option changes how [New] builds the node.
type Option func(*options)

type options struct {
	onCommit func(time.Duration)
}

// WithCommitObserver sets a function that is told how long each write this node
// proposes took to commit and apply, as the leader does: from the proposal to the
// answer of raft, a failed write included. It runs on the goroutine of the write,
// which holds the store's transaction lock, so it must not call the store.
func WithCommitObserver(fn func(time.Duration)) Option {
	return func(o *options) { o.onCommit = fn }
}

// ErrDataDirNotPrivate indicates a data directory, or a snapshots directory in
// it, that its group or others can enter, or that is not a directory but a
// symlink or another file.
var ErrDataDirNotPrivate = errors.New("the raft data directory and its snapshots directory must be directories that only their owner can enter (mode 0700), not symlinks")

// newRaft starts hashicorp/raft on the stores New prepared. It is a variable
// so that tests can see what the stores hold when raft starts.
var newRaft = hraft.NewRaft

// identityMismatch is the ERROR of a node with raft state whose stored
// configuration holds no server with its node_id at its advertise address (see
// checkIdentity).
const identityMismatch = "raft: this node is not in its stored raft configuration as its node_id at its advertise_addr, and the cluster knows its nodes by the IDs and addresses of that configuration (servers): under another node_id this node is never elected, and at another advertise_addr its peers still dial the stored address. A node renamed or re-addressed since its raft state was written must be removed from the cluster, its data_dir wiped, and added again with cluster.raft.join: true"

// Profile bundles the Raft-backed storage.Storage with a cluster.Leadership.
type Profile struct {
	cfg Config

	inner      storage.Storage
	store      *Store
	hub        *event.Hub
	leadership *Leadership
	raft       *hraft.Raft
	transport  *hraft.NetworkTransport
	security   *transportTLS // the transport's mutual TLS, nil on plain TCP; Admin probes with it
	logStore   *boltdb.BoltStore
	stable     *boltdb.BoltStore
	snap       hraft.SnapshotStore

	logger zerolog.Logger
}

// New builds and starts a Raft profile. It expects a configuration that
// [cluster.Config.Validate] accepted, and checks only that NodeID, BindAddr
// and DataDir are set, that the transport has TLS or may run plain TCP, and
// that hashicorp/raft accepts the timings. It loads the TLS files and checks
// the node's certificate before it makes DataDir, which it makes 0700, as its
// snapshots directory, and refuses, before it opens anything there, when either
// is open to its group or others (see privateDir). A caller that skipped
// Validate and advertises every interface (0.0.0.0, ::) or no host gets
// ErrNodeCertificate over TLS, as the node's certificate is checked for that
// host first, and errNotAdvertisable on plain TCP, before the transport
// listens.
//
// On first start (empty DataDir) the node derives its server list from
// disco.Peers() (see bootstrapServers) before its transport listens, and
// writes it into its stores as the bootstrap configuration before raft runs.
// A node that joins (Config.Join) does neither on its first start: it starts
// raft with no configuration, and the leader of a running cluster adds it. On
// subsequent starts the on-disk state is loaded and the cluster re-formed
// automatically, without asking disco, join or not; the node checks that its
// stored configuration holds it as NodeID at its advertise address, and logs an
// ERROR when not (checkIdentity).
func New(cfg Config, inner storage.Storage, hub *event.Hub, disco cluster.Discovery, logger zerolog.Logger, opts ...Option) (*Profile, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("raft: NodeID is required")
	}
	if cfg.BindAddr == "" {
		return nil, fmt.Errorf("raft: BindAddr is required")
	}
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("raft: DataDir is required")
	}
	if inner == nil {
		return nil, fmt.Errorf("raft: nil inner store")
	}
	if hub == nil {
		return nil, fmt.Errorf("raft: nil hub")
	}
	advertise := cfg.AdvertiseAddr
	if advertise == "" {
		advertise = cfg.BindAddr
	}
	security, err := transportSecurity(cfg, advertise)
	if err != nil {
		return nil, err
	}
	logger = logger.With().Str("profile", "raft").Logger()

	rcfg := raftConfig(cfg, logger)
	// hashicorp/raft checks its settings only once BootstrapCluster or NewRaft
	// runs, after the stores open and the transport listens, and names its own
	// fields: the error names the settings an operator writes, before anything
	// is made. cluster.Config.Validate refuses the same values earlier.
	if err := hraft.ValidateConfig(rcfg); err != nil {
		return nil, fmt.Errorf("raft: hashicorp/raft refuses the timing settings (cluster.raft.election_timeout_ms, heartbeat_timeout_ms, snapshot_interval): %w", err)
	}

	for _, dir := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, snapshotsDir)} {
		if err := privateDir(dir); err != nil {
			return nil, err
		}
	}

	// The advertise address is resolved only to check that peers can dial it.
	// The transport gives raft the address as written, as the bootstrap
	// configuration, the discovered list, names this node, so the node's own
	// address and its leader address are the ones its peers know, whichever
	// IP its local DNS returned first.
	advTCP, err := net.ResolveTCPAddr("tcp", advertise)
	if err != nil {
		return nil, fmt.Errorf("raft: resolve advertise %q: %w", advertise, err)
	}
	// Inspect existing commands with a read-only database handle before any
	// writable stores or Raft goroutines can initialize/replay legacy state.
	if err := preflightLogFile(filepath.Join(cfg.DataDir, "raft-log.bolt")); err != nil {
		return nil, err
	}
	snap, err := newSnapshotStore(cfg.DataDir, logger)
	if err != nil {
		return nil, err
	}
	if err := preflightSnapshots(snap); err != nil {
		return nil, err
	}

	var (
		transport        *hraft.NetworkTransport
		logStore, stable *boltdb.BoltStore
		r                *hraft.Raft
	)
	constructed := false
	defer func() {
		if constructed {
			return
		}
		if r != nil {
			_ = r.Shutdown().Error()
		}
		if stable != nil {
			_ = stable.Close()
		}
		if logStore != nil {
			_ = logStore.Close()
		}
		if transport != nil {
			_ = transport.Close()
		}
	}()

	logStore, err = boltdb.New(boltdb.Options{Path: filepath.Join(cfg.DataDir, "raft-log.bolt"), BoltOptions: &bolt.Options{Timeout: 250 * time.Millisecond}})
	if err != nil {
		return nil, fmt.Errorf("raft: open log store: %w", err)
	}
	stable, err = boltdb.New(boltdb.Options{Path: filepath.Join(cfg.DataDir, "raft-stable.bolt"), BoltOptions: &bolt.Options{Timeout: 250 * time.Millisecond}})
	if err != nil {
		return nil, fmt.Errorf("raft: open stable store: %w", err)
	}
	// Recheck after the read-only handle was closed and the exclusive write
	// lock acquired: another process cannot change log state during replay.
	if err := preflightLogs(logStore); err != nil {
		return nil, err
	}
	hasState, err := hraft.HasExistingState(logStore, stable, snap)
	if err != nil {
		return nil, fmt.Errorf("raft: has existing state: %w", err)
	}
	if hasState {
		checkIdentity(cfg.NodeID, advertise, logStore, snap, logger)
	}
	// Bootstrap if log is empty (first start in this DataDir), unless the node
	// joins a running cluster. The server list comes first, so a list this node
	// cannot bootstrap with fails the start before the transport listens and
	// before the node keeps any state.
	bootstrap := !hasState && !cfg.Join
	var servers []hraft.Server
	if bootstrap {
		if servers, err = bootstrapServers(cfg, hraft.ServerAddress(advertise), disco); err != nil {
			return nil, err
		}
	}
	transport, err = newTransport(cfg.BindAddr, advertise, advTCP, security, configAnswer(cfg.ConfigDigests), logger)
	if err != nil {
		return nil, fmt.Errorf("raft: transport on %s: %w", cfg.BindAddr, err)
	}
	if security == nil {
		logger.Warn().Str("bind_addr", cfg.BindAddr).
			Msg("cluster.raft.allow_insecure_transport is true: the raft transport runs plain TCP, so any host that reaches bind_addr can read raft traffic, agent tokens and password verifiers included, and forge it; set cluster.raft.tls for mutual TLS")
	}
	// The bootstrap configuration goes into the stores before raft runs. The
	// transport listens already, but nothing consumes its RPCs before NewRaft,
	// so no vote request can persist a term first, which would give the node
	// state and make the bootstrap fail with ErrCantBootstrap.
	if bootstrap {
		if err := hraft.BootstrapCluster(rcfg, logStore, stable, snap, transport, hraft.Configuration{Servers: servers}); err != nil {
			return nil, fmt.Errorf("raft: bootstrap cluster: %w", err)
		}
	}
	if !hasState && cfg.Join {
		logger.Info().Str("node_id", cfg.NodeID).Str("advertise_addr", advertise).
			Msg("cluster.raft.join is true and the node has no raft state: it bootstraps nothing, and waits for the leader to add it (POST /api/v1/cluster/members)")
	}

	fsm := NewFSM(inner, hub, logger)
	r, err = newRaft(rcfg, fsm, logStore, stable, snap, transport)
	if err != nil {
		return nil, fmt.Errorf("raft: new raft: %w", err)
	}

	p := &Profile{
		cfg:        cfg,
		inner:      inner,
		hub:        hub,
		raft:       r,
		transport:  transport,
		security:   security,
		logStore:   logStore,
		stable:     stable,
		snap:       snap,
		logger:     logger,
		leadership: NewLeadership(r, logger),
	}
	p.store = NewStore(inner, r)
	p.store.onCommit = o.onCommit
	// Plug the event-writer so hub mutations replicate through Raft. From
	// this point Notify/Record/DeleteIssues on any replica go via the log;
	// the FSM fires ApplyLocal* on every node.
	hub.SetWriter(NewEventWriter(r, hub))
	constructed = true
	return p, nil
}

// raftConfig is the hashicorp/raft configuration of cfg: hashicorp/raft's
// defaults, with the timings and the compaction settings that cfg sets, and the
// node's logger in place of hashicorp/raft's own on stderr.
func raftConfig(cfg Config, logger zerolog.Logger) *hraft.Config {
	rcfg := hraft.DefaultConfig()
	rcfg.LocalID = hraft.ServerID(cfg.NodeID)
	if cfg.ElectionTimeout > 0 {
		rcfg.ElectionTimeout = cfg.ElectionTimeout
	}
	if cfg.HeartbeatTimeout > 0 {
		rcfg.HeartbeatTimeout = cfg.HeartbeatTimeout
	}
	if cfg.SnapshotInterval > 0 {
		rcfg.SnapshotInterval = cfg.SnapshotInterval
	}
	if cfg.SnapshotThreshold > 0 {
		rcfg.SnapshotThreshold = cfg.SnapshotThreshold
	}
	if cfg.TrailingLogs > 0 {
		rcfg.TrailingLogs = cfg.TrailingLogs
	}
	// LeaderLeaseTimeout must be <= HeartbeatTimeout; Raft rejects the
	// config otherwise. Default is 500ms — when the caller tightens
	// heartbeats (e.g. tests with 50ms) we must bring the lease down too.
	if rcfg.LeaderLeaseTimeout > rcfg.HeartbeatTimeout {
		rcfg.LeaderLeaseTimeout = rcfg.HeartbeatTimeout
	}
	// hashicorp/raft logs through the node's logger, in place of its own on
	// stderr, as do the snapshot store and the transport New opens.
	rcfg.Logger = newHCLog(logger, "raft")
	return rcfg
}

// checkIdentity logs one ERROR, identityMismatch, when the latest
// configuration stored in logs and snaps holds no server with nodeID at
// advertise, compared as written: the node of a restart under another
// node_id, or at another advertise address, which its peers know by the ID and
// the address of that configuration. hashicorp/raft makes a candidate only of a
// voter of its configuration, by ID (runFollower in v1.8.0, which logs a WARN
// "not part of stable configuration, aborting election"), and its peers dial a
// member at its stored address. It fails nothing, so that no edge case keeps a
// node down. A stored configuration without a server, that of a joining node
// the leader has not reached, logs nothing; stores it cannot read log a WARN,
// and NewRaft reads them next.
func checkIdentity(nodeID, advertise string, logs hraft.LogStore, snaps hraft.SnapshotStore, logger zerolog.Logger) {
	conf, _, err := latestConfiguration(logs, snaps)
	if err != nil {
		logger.Warn().Err(err).Str("node_id", nodeID).Str("advertise_addr", advertise).
			Msg("raft: cannot read the stored configuration to check this node's node_id and advertise address against it")
		return
	}
	if len(conf.Servers) == 0 {
		return
	}
	servers := make([]string, 0, len(conf.Servers))
	for _, s := range conf.Servers {
		if string(s.ID) == nodeID && string(s.Address) == advertise {
			return
		}
		servers = append(servers, string(s.ID)+"="+string(s.Address))
	}
	logger.Error().Str("node_id", nodeID).Str("advertise_addr", advertise).Strs("servers", servers).Msg(identityMismatch)
}

// Store returns the Raft-backed storage.Storage.
func (p *Profile) Store() storage.Storage { return p.store }

// Leadership returns the raft-based Leadership.
func (p *Profile) Leadership() cluster.Leadership { return p.leadership }

// Hub returns the local event.Hub. Writes go through Raft (see EventWriter);
// reads are local. Exposed primarily for tests and main-wiring.
func (p *Profile) Hub() *event.Hub { return p.hub }

// Shutdown gracefully stops the raft node and releases log/stable stores.
func (p *Profile) Shutdown() error {
	if p.raft != nil {
		if err := p.raft.Shutdown().Error(); err != nil {
			return fmt.Errorf("raft: shutdown: %w", err)
		}
	}
	if p.logStore != nil {
		_ = p.logStore.Close()
	}
	if p.stable != nil {
		_ = p.stable.Close()
	}
	if p.transport != nil {
		_ = p.transport.Close()
	}
	return nil
}

// snapshotsDir is the directory of the data directory that hashicorp/raft's
// file snapshot store keeps its snapshots in.
const snapshotsDir = "snapshots"

// privateDir makes dir 0700 when it is missing and refuses it when it is not a
// directory, a symlink included, or its group or others have any permission on
// it. The path is cleaned first: with a trailing slash, Lstat would follow a
// symlink. It checks no file: BoltDB makes its files 0600, and the snapshot
// store makes its own private.
func privateDir(dir string) error {
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("raft: mkdir %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("raft: stat %s: %w", dir, err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("raft: %s has mode %v: %w", dir, info.Mode(), ErrDataDirNotPrivate)
	}
	return nil
}

// snapshotStore is hashicorp/raft's file snapshot store with private
// snapshots: each snapshot directory 0700 and its files 0600, from Create on
// and again once Close has put the snapshot in place.
type snapshotStore struct {
	*hraft.FileSnapshotStore
	dir string // the snapshots directory
}

// newSnapshotStore opens the snapshot store of dataDir, whose snapshots
// directory must be as private as dataDir, and makes private the snapshots it
// holds already: those of an earlier version, which left them 0755 and 0644,
// or of a snapshot that a crash cut short. New checks the directory earlier,
// before anything is opened; the check here keeps the store private for any
// other caller. The store logs into logger.
func newSnapshotStore(dataDir string, logger zerolog.Logger) (*snapshotStore, error) {
	dir := filepath.Join(dataDir, snapshotsDir)
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	if err := privateSnapshots(dir); err != nil {
		return nil, err
	}
	store, err := hraft.NewFileSnapshotStoreWithLogger(dataDir, 2, newHCLog(logger, "snapshot"))
	if err != nil {
		return nil, fmt.Errorf("raft: snapshot store: %w", err)
	}
	return &snapshotStore{FileSnapshotStore: store, dir: dir}, nil
}

// Create implements hraft.SnapshotStore. A snapshot the node takes, and one a
// leader installs on it, both start here.
func (s *snapshotStore) Create(version hraft.SnapshotVersion, index, term uint64, configuration hraft.Configuration, configurationIndex uint64, trans hraft.Transport) (hraft.SnapshotSink, error) {
	sink, err := s.FileSnapshotStore.Create(version, index, term, configuration, configurationIndex, trans)
	if err != nil {
		return nil, err
	}
	if err := privateSnapshots(s.dir); err != nil {
		_ = sink.Cancel()
		return nil, err
	}
	return &snapshotSink{SnapshotSink: sink, dir: s.dir}, nil
}

// snapshotSink is a sink of a snapshotStore.
type snapshotSink struct {
	hraft.SnapshotSink
	dir string // the snapshots directory
}

// Close implements hraft.SnapshotSink: it persists the snapshot, which
// hashicorp/raft then moves into place, and makes it private again.
func (s *snapshotSink) Close() error {
	if err := s.SnapshotSink.Close(); err != nil {
		return err
	}
	return privateSnapshots(s.dir)
}

// privateSnapshots takes the group and other permissions off everything under
// dir, the snapshots directory. It leaves symlinks alone, which raft ignores,
// and skips an entry that a reap removes meanwhile.
func privateSnapshots(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case path == dir || d.Type()&fs.ModeSymlink != 0:
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			if err := os.Chmod(path, perm&^0o077); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("raft: make snapshot %s private: %w", path, err)
			}
		}
		return nil
	})
}
