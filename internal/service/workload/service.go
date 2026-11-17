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

// The refusals the service remembers: at most maxRefused fingerprints, each
// with its reason cut to maxRefusalBytes.
const (
	maxRefused      = 4096
	maxRefusalBytes = 512
)

// maxLoggedFingerprintBytes bounds an offered fingerprint in a log line: a real
// one is 64 bytes, but an agent may offer a schema under any string.
const maxLoggedFingerprintBytes = 128

// RefusedError is what Compiled returns for a schema this podium refuses: one
// whose bytes match its fingerprint and that Compile refuses, offered by an
// agent or stored in the catalog by another version. The refusal is
// deterministic for this binary. Reason says why, cut to 512 bytes; it quotes
// agent-supplied schema text.
type RefusedError struct {
	Fingerprint string
	Reason      string
}

func (e *RefusedError) Error() string {
	return "workload: podium refused the schema " + e.Fingerprint + ": " + e.Reason
}

// Service is the workload catalog.
type Service struct {
	store  storage.Storage
	logger zerolog.Logger
	clock  func() time.Time

	mu       sync.Mutex
	compiled map[string]*workloadschema.Schema
	// known holds the fingerprints seen in the catalog. An entry never changes
	// or goes, so a fingerprint seen once never hits the store again.
	known map[string]struct{}
	// refused holds, by fingerprint, why this process refuses a schema whose
	// bytes match the fingerprint: a refusal that is the same on every try. At
	// most maxRefused entries; once full, a new refusal is not remembered, and
	// its fingerprint is asked for and refused again, as each is the first time.
	// An entry never changes or goes, but with the process.
	refused map[string]string
}

// New returns the catalog service over store.
func New(store storage.Storage, logger zerolog.Logger) *Service {
	return &Service{
		store:    store,
		logger:   logger.With().Str("service", "workload").Logger(),
		clock:    time.Now,
		compiled: make(map[string]*workloadschema.Schema),
		known:    make(map[string]struct{}),
		refused:  make(map[string]string),
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
		if _, refused := s.Refusal(o.Fingerprint); refused {
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
		if _, err := workloadschema.Compile(o.Schema); err != nil {
			s.refuse(agentID, fp, err)
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
		if _, refused := s.Refusal(fp); refused {
			continue
		}
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

// Refusal returns why this podium refuses the schema of fingerprint, and false
// for one it does not refuse, or no longer remembers refusing.
func (s *Service) Refusal(fingerprint string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reason, ok := s.refused[fingerprint]
	return reason, ok
}

// refuse remembers that this podium refuses the schema of fingerprint, whose
// bytes match it, for the reason err gives, cut to maxRefusalBytes, and returns
// the reason. The refusal is logged once, at WARN, the reason in a field: when
// it is remembered, or every time while the refusals kept are at their bound.
// agentID is the agent that offered the schema, empty for one in the catalog.
func (s *Service) refuse(agentID, fingerprint string, err error) string {
	reason := capText(err.Error(), maxRefusalBytes)
	s.mu.Lock()
	_, had := s.refused[fingerprint]
	kept := had || len(s.refused) < maxRefused
	if kept && !had {
		s.refused[fingerprint] = reason
	}
	s.mu.Unlock()
	if had {
		return reason
	}
	ev := s.logger.Warn().Str("fingerprint", fingerprint).Str("reason", reason)
	if agentID != "" {
		ev = ev.Str("agent_id", agentID)
	}
	switch {
	case !kept:
		ev.Int("refused", maxRefused).Msg("workload schema refused; the refusals kept are at their bound, so podium asks for it again")
	case agentID == "":
		ev.Msg("stored workload schema refused: this podium does not compile it")
	default:
		ev.Msg("workload schema refused; podium no longer asks for it")
	}
	return reason
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

// Compiled returns the compiled schema of a fingerprint in the catalog, and
// false for one that is not in it. Compiled schemas are cached by fingerprint;
// a fingerprint that is not in the catalog is not cached, as it can arrive. A
// schema this podium refuses, offered or stored, is a *RefusedError.
func (s *Service) Compiled(ctx context.Context, fingerprint string) (*workloadschema.Schema, bool, error) {
	s.mu.Lock()
	c, ok := s.compiled[fingerprint]
	reason, refused := s.refused[fingerprint]
	s.mu.Unlock()
	if ok {
		return c, true, nil
	}
	if refused {
		return nil, false, &RefusedError{Fingerprint: fingerprint, Reason: reason}
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
		return nil, false, &RefusedError{Fingerprint: fingerprint, Reason: s.refuse("", fingerprint, err)}
	}
	s.mu.Lock()
	s.compiled[fingerprint] = c
	s.mu.Unlock()
	return c, true, nil
}
