// Package raft wires hashicorp/raft on top of the inmemory store.
//
// Usage:
//
//	profile, err := raft.New(ctx, raft.Config{...}, inmemory.New(), disco, logger)
//	store := profile.Store()                 // storage.Storage wired through raft
//	leadership := profile.Leadership()       // cluster.Leadership
//
// Transport: TCP.
// Log store: BoltDB on disk (so logs survive restart).
// Peer bootstrap: from [cluster.Discovery] — on first start we bootstrap the
// cluster with every peer Discovery reports. Subsequent starts join a
// pre-existing cluster and reuse the on-disk log.
package raft

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
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
	// BindAddr is used.
	AdvertiseAddr string

	// DataDir holds the BoltDB log store and snapshots. Required.
	DataDir string

	// ElectionTimeout / HeartbeatTimeout tune the Raft timings. Zero
	// values use hashicorp/raft defaults (1s / 1s). For in-DC deployments
	// you can lower to 100-200ms; for cross-DC bump to 3-5s.
	ElectionTimeout  time.Duration
	HeartbeatTimeout time.Duration
}

// Profile bundles the Raft-backed storage.Storage with a cluster.Leadership.
type Profile struct {
	cfg Config

	inner      storage.Storage
	store      *Store
	leadership *Leadership
	raft       *hraft.Raft
	transport  *hraft.NetworkTransport
	logStore   *boltdb.BoltStore
	stable     *boltdb.BoltStore
	snap       hraft.SnapshotStore

	logger zerolog.Logger
}

// New builds and starts a Raft profile.
//
// On first start (empty DataDir) the node bootstraps a cluster consisting of
// every peer returned by disco.Peers() plus itself. On subsequent starts the
// on-disk state is loaded and the cluster re-formed automatically.
func New(cfg Config, inner storage.Storage, disco cluster.Discovery, logger zerolog.Logger) (*Profile, error) {
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

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("raft: mkdir %s: %w", cfg.DataDir, err)
	}

	rcfg := hraft.DefaultConfig()
	rcfg.LocalID = hraft.ServerID(cfg.NodeID)
	if cfg.ElectionTimeout > 0 {
		rcfg.ElectionTimeout = cfg.ElectionTimeout
	}
	if cfg.HeartbeatTimeout > 0 {
		rcfg.HeartbeatTimeout = cfg.HeartbeatTimeout
	}
	// LeaderLeaseTimeout must be <= HeartbeatTimeout; Raft rejects the
	// config otherwise. Default is 500ms — when the caller tightens
	// heartbeats (e.g. tests with 50ms) we must bring the lease down too.
	if rcfg.LeaderLeaseTimeout > rcfg.HeartbeatTimeout {
		rcfg.LeaderLeaseTimeout = rcfg.HeartbeatTimeout
	}
	rcfg.LogOutput = os.Stderr

	advertise := cfg.AdvertiseAddr
	if advertise == "" {
		advertise = cfg.BindAddr
	}
	// TCP transport requires a *net.TCPAddr for advertise — it validates
	// the concrete type. We resolve the FQDN to an IP for the transport,
	// but for the Raft *bootstrap configuration* we keep the raw string
	// so every replica bootstraps with the same config regardless of
	// which IP its local DNS returned first.
	advTCP, err := net.ResolveTCPAddr("tcp", advertise)
	if err != nil {
		return nil, fmt.Errorf("raft: resolve advertise %q: %w", advertise, err)
	}
	transport, err := hraft.NewTCPTransport(cfg.BindAddr, advTCP, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft: tcp transport on %s: %w", cfg.BindAddr, err)
	}

	logStore, err := boltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-log.bolt"))
	if err != nil {
		return nil, fmt.Errorf("raft: open log store: %w", err)
	}
	stable, err := boltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-stable.bolt"))
	if err != nil {
		return nil, fmt.Errorf("raft: open stable store: %w", err)
	}
	snap, err := hraft.NewFileSnapshotStore(cfg.DataDir, 2, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft: snapshot store: %w", err)
	}

	fsm := NewFSM(inner)
	r, err := hraft.NewRaft(rcfg, fsm, logStore, stable, snap, transport)
	if err != nil {
		return nil, fmt.Errorf("raft: new raft: %w", err)
	}

	// Bootstrap if log is empty (first start in this DataDir).
	hasState, err := hraft.HasExistingState(logStore, stable, snap)
	if err != nil {
		return nil, fmt.Errorf("raft: has existing state: %w", err)
	}
	if !hasState {
		// Use the CONFIG advertise string (FQDN as written by the user)
		// for the bootstrap config, not transport.LocalAddr() — that one
		// is IP-resolved and would differ across replicas, breaking the
		// "all replicas bootstrap with identical config" requirement.
		servers, err := bootstrapServers(cfg, hraft.ServerAddress(advertise), disco)
		if err != nil {
			return nil, err
		}
		if err := r.BootstrapCluster(hraft.Configuration{Servers: servers}).Error(); err != nil {
			return nil, fmt.Errorf("raft: bootstrap cluster: %w", err)
		}
	}

	p := &Profile{
		cfg:        cfg,
		inner:      inner,
		raft:       r,
		transport:  transport,
		logStore:   logStore,
		stable:     stable,
		snap:       snap,
		logger:     logger.With().Str("profile", "raft").Logger(),
		leadership: NewLeadership(r),
	}
	p.store = NewStore(inner, r)
	return p, nil
}

// Store returns the Raft-backed storage.Storage.
func (p *Profile) Store() storage.Storage { return p.store }

// Leadership returns the raft-based Leadership.
func (p *Profile) Leadership() cluster.Leadership { return p.leadership }

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
