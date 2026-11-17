// Package workload is the catalog of workload schemas: it takes in the schemas
// that agents offer for the workload types they declare, and compiles the ones
// in the catalog.
package workload

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/workloadschema"
)

// OfferedSchema is one schema an agent sends with a sync: the fingerprint it
// claims for it and the schema bytes.
type OfferedSchema struct {
	Fingerprint string
	Schema      []byte
}

// Service is the workload catalog.
type Service struct {
	store  storage.Storage
	logger zerolog.Logger
	clock  func() time.Time

	mu       sync.Mutex
	compiled map[string]*workloadschema.Schema
}

// New returns the catalog service over store.
func New(store storage.Storage, logger zerolog.Logger) *Service {
	return &Service{
		store:    store,
		logger:   logger.With().Str("service", "workload").Logger(),
		clock:    time.Now,
		compiled: make(map[string]*workloadschema.Schema),
	}
}

// declaredSchema is a fingerprint an agent declares, with the workload type
// that declares it.
type declaredSchema struct {
	apiVersion, kind string
}

// Exchange is the schema exchange of one sync. It stores each offered schema
// that the agent declares in declared, that is at most MaxSchemaBytes, whose
// fingerprint is the one it is offered under and that compiles, under the
// workload type that declares it (the first agent to offer a fingerprint
// wins). Every other offered schema is logged with the agent ID and dropped.
// It returns the fingerprints in declared that the catalog does not hold, sorted
// and without duplicates, for the agent to send on its next sync.
//
// It never fails the sync: a store error is logged and its fingerprint stays
// wanted. A sync that offers nothing new only reads.
func (s *Service) Exchange(ctx context.Context, agentID string, declared []model.RunnerCapability, offered []OfferedSchema) []string {
	decl := make(map[string]declaredSchema)
	var order []string
	for _, r := range declared {
		for _, w := range r.Workloads {
			fp := w.SchemaFingerprint
			if fp == "" {
				continue
			}
			if _, dup := decl[fp]; dup {
				continue
			}
			apiVersion := w.Version
			if w.Group != "" {
				apiVersion = w.Group + "/" + w.Version
			}
			decl[fp] = declaredSchema{apiVersion: apiVersion, kind: w.Kind}
			order = append(order, fp)
		}
	}

	stored := make(map[string]bool)
	for _, o := range offered {
		d, ok := decl[o.Fingerprint]
		if !ok {
			s.reject(agentID, o.Fingerprint, "the schema is not declared by the agent")
			continue
		}
		if stored[o.Fingerprint] {
			continue
		}
		if _, err := s.store.GetWorkloadSchema(ctx, o.Fingerprint); err == nil {
			continue
		} else if !errors.Is(err, storage.ErrNotFound) {
			s.logger.Error().Err(err).Str("agent_id", agentID).Str("fingerprint", o.Fingerprint).Msg("workload schema lookup failed")
			continue
		}
		if len(o.Schema) > workloadschema.MaxSchemaBytes {
			s.reject(agentID, o.Fingerprint, "the schema is over the size limit")
			continue
		}
		fp, err := workloadschema.Fingerprint(o.Schema)
		if err != nil {
			s.reject(agentID, o.Fingerprint, "the schema is not canonical JSON: "+err.Error())
			continue
		}
		if fp != o.Fingerprint {
			s.reject(agentID, o.Fingerprint, "the fingerprint does not match the schema")
			continue
		}
		if _, err := workloadschema.Compile(o.Schema); err != nil {
			s.reject(agentID, o.Fingerprint, "the schema does not compile: "+err.Error())
			continue
		}
		entry, err := model.NewWorkloadSchema(fp, d.apiVersion, d.kind, o.Schema, agentID, s.clock())
		if err != nil {
			s.reject(agentID, o.Fingerprint, err.Error())
			continue
		}
		if err := s.store.UpsertWorkloadSchema(ctx, entry); err != nil {
			s.logger.Error().Err(err).Str("agent_id", agentID).Str("fingerprint", fp).Msg("workload schema store failed")
			continue
		}
		stored[fp] = true
	}

	var want []string
	for _, fp := range order {
		if stored[fp] {
			continue
		}
		if _, err := s.store.GetWorkloadSchema(ctx, fp); err == nil {
			continue
		} else if !errors.Is(err, storage.ErrNotFound) {
			s.logger.Error().Err(err).Str("agent_id", agentID).Str("fingerprint", fp).Msg("workload schema lookup failed")
		}
		want = append(want, fp)
	}
	slices.Sort(want)
	return want
}

func (s *Service) reject(agentID, fingerprint, why string) {
	s.logger.Warn().Str("agent_id", agentID).Str("fingerprint", fingerprint).Msg("workload schema rejected: " + why)
}

// Compiled returns the compiled schema of a fingerprint in the catalog, and
// false for one that is not in it. Compiled schemas are cached by fingerprint;
// a fingerprint that is not in the catalog is not cached, as it can arrive.
func (s *Service) Compiled(ctx context.Context, fingerprint string) (*workloadschema.Schema, bool, error) {
	s.mu.Lock()
	c, ok := s.compiled[fingerprint]
	s.mu.Unlock()
	if ok {
		return c, true, nil
	}
	entry, err := s.store.GetWorkloadSchema(ctx, fingerprint)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	c, err = workloadschema.Compile(entry.Schema())
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	s.compiled[fingerprint] = c
	s.mu.Unlock()
	return c, true, nil
}
