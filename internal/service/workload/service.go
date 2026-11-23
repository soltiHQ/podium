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
	"unicode/utf8"

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

// maxRefusalBytes bounds the reason a rejected schema logs.
const maxRefusalBytes = 512

// maxLoggedFingerprintBytes bounds an offered fingerprint in a log line: a real
// one is 64 bytes, but an agent may offer a schema under any string.
const maxLoggedFingerprintBytes = 128

// Service is the workload catalog.
type Service struct {
	store  storage.Storage
	logger zerolog.Logger
	clock  func() time.Time

	mu sync.Mutex
	// known holds the fingerprints seen in the catalog. An entry never changes
	// or goes, so a fingerprint seen once never hits the store again.
	known map[string]struct{}
}

// New returns the catalog service over store.
func New(store storage.Storage, logger zerolog.Logger) *Service {
	return &Service{
		store:  store,
		logger: logger.With().Str("service", "workload").Logger(),
		clock:  time.Now,
		known:  make(map[string]struct{}),
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
// wins). A schema whose fingerprint is right and that does not compile is
// refused for good: it is remembered, see Refusal, and logged once. Every other
// offered schema is logged with the agent ID and dropped. It returns the
// fingerprints in declared that the catalog does not hold and that this podium
// does not refuse, sorted and without duplicates, for the agent to send on its
// next sync.
//
// It never fails the sync: a store error is logged and its fingerprint stays
// wanted. A sync that offers nothing new only reads, and reads nothing for a
// refused fingerprint. What the agent sent reaches the logs only in fields, cut.
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

	// absent memoizes the fingerprints this call found missing or failed to
	// look up, so each is read, and a failure logged, once.
	absent := make(map[string]bool)
	seen := make(map[string]bool)
	for _, o := range offered {
		if seen[o.Fingerprint] {
			continue
		}
		seen[o.Fingerprint] = true
		d, ok := decl[o.Fingerprint]
		if !ok {
			s.reject(agentID, o.Fingerprint, "the schema is not declared by the agent")
			continue
		}
		if s.has(ctx, agentID, o.Fingerprint, absent) {
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
		entry, err := model.NewWorkloadSchema(fp, d.apiVersion, d.kind, o.Schema, agentID, s.clock())
		if err != nil {
			s.reject(agentID, o.Fingerprint, err.Error())
			continue
		}
		if err := s.store.UpsertWorkloadSchema(ctx, entry); err != nil {
			s.logger.Error().Err(err).Str("agent_id", agentID).Str("fingerprint", fp).Msg("workload schema store failed")
			continue
		}
		s.markKnown(fp)
	}

	var want []string
	for _, fp := range order {
		if !s.has(ctx, agentID, fp, absent) {
			want = append(want, fp)
		}
	}
	slices.Sort(want)
	return want
}

// has reports whether the catalog holds fingerprint. A fingerprint seen once
// is answered from memory; one missing, or whose lookup failed (logged), is
// memoized in absent for the rest of the call only, as it can arrive.
func (s *Service) has(ctx context.Context, agentID, fingerprint string, absent map[string]bool) bool {
	s.mu.Lock()
	_, ok := s.known[fingerprint]
	s.mu.Unlock()
	if ok {
		return true
	}
	if absent[fingerprint] {
		return false
	}
	_, err := s.store.GetWorkloadSchema(ctx, fingerprint)
	switch {
	case err == nil:
		s.markKnown(fingerprint)
		return true
	case !errors.Is(err, storage.ErrNotFound):
		s.logger.Error().Err(err).Str("agent_id", agentID).Str("fingerprint", fingerprint).Msg("workload schema lookup failed")
	}
	absent[fingerprint] = true
	return false
}

func (s *Service) markKnown(fingerprint string) {
	s.mu.Lock()
	s.known[fingerprint] = struct{}{}
	s.mu.Unlock()
}

// reject logs an offered schema that is dropped. The fingerprint and the
// reason may hold agent text: they go in fields, cut.
func (s *Service) reject(agentID, fingerprint, reason string) {
	s.logger.Warn().
		Str("agent_id", agentID).
		Str("fingerprint", capText(fingerprint, maxLoggedFingerprintBytes)).
		Str("reason", capText(reason, maxRefusalBytes)).
		Msg("workload schema rejected")
}

// capText cuts s to at most limit bytes on a UTF-8 boundary, and ends a cut
// text with a marker, as proxy.CapAgentText does for an agent's error.
func capText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for i := 0; i < utf8.UTFMax && cut > 0 && !utf8.RuneStart(s[cut]); i++ {
		cut--
	}
	return s[:cut] + "…"
}
