package workload

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// TypeView is a workload type: the one an agent declares, or one whose schema
// the catalog holds.
type TypeView struct {
	APIVersion, Kind string
	Fingerprints     []FingerprintView
}

// FingerprintView is a schema fingerprint of a workload type: the agents that
// declare it, and its schema once it has arrived in the catalog. Schema is nil
// and FirstSeenAt zero until then. Refused is why this podium refuses the
// schema (see Service.Refusal), empty for one it does not refuse: a schema it
// refused when an agent offered it never arrives, and one in the catalog that it
// cannot compile shows as refused once podium has tried to compile it.
type FingerprintView struct {
	Fingerprint string
	Agents      []string
	Schema      []byte
	FirstSeenAt time.Time
	Refused     string
}

type typeKey struct{ apiVersion, kind string }

// Types returns every workload type that an agent declares or that has a schema
// in the catalog, sorted by API version and kind, with its fingerprints sorted.
// A type that agents declare without a schema fingerprint has none.
func (s *Service) Types(ctx context.Context) ([]TypeView, error) {
	types, err := s.collect(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]typeKey, 0, len(types))
	for k := range types {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b typeKey) int {
		if c := strings.Compare(a.apiVersion, b.apiVersion); c != 0 {
			return c
		}
		return strings.Compare(a.kind, b.kind)
	})
	out := make([]TypeView, 0, len(keys))
	for _, k := range keys {
		out = append(out, types[k])
	}
	return out, nil
}

// Type returns one workload type, or storage.ErrNotFound when no agent declares
// it and the catalog holds no schema for it.
func (s *Service) Type(ctx context.Context, apiVersion, kind string) (TypeView, error) {
	types, err := s.collect(ctx)
	if err != nil {
		return TypeView{}, err
	}
	v, ok := types[typeKey{apiVersion, kind}]
	if !ok {
		return TypeView{}, storage.ErrNotFound
	}
	return v, nil
}

func (s *Service) collect(ctx context.Context) (map[typeKey]TypeView, error) {
	agents, err := storage.AllAgents(ctx, s.store, nil)
	if err != nil {
		return nil, err
	}
	entries, err := storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.WorkloadSchemaListResult, error) {
		return s.store.ListWorkloadSchemas(ctx, o)
	})
	if err != nil {
		return nil, err
	}
	byFingerprint := make(map[string]*model.WorkloadSchema, len(entries))
	for _, e := range entries {
		byFingerprint[e.ID()] = e
	}

	declared := make(map[typeKey]map[string]map[string]bool)
	add := func(k typeKey, fp, agentID string) {
		if declared[k] == nil {
			declared[k] = make(map[string]map[string]bool)
		}
		if fp == "" {
			return
		}
		if declared[k][fp] == nil {
			declared[k][fp] = make(map[string]bool)
		}
		if agentID != "" {
			declared[k][fp][agentID] = true
		}
	}
	for _, a := range agents {
		for _, r := range a.Capabilities() {
			for _, w := range r.Workloads {
				apiVersion := w.Version
				if w.Group != "" {
					apiVersion = w.Group + "/" + w.Version
				}
				add(typeKey{apiVersion, w.Kind}, w.SchemaFingerprint, a.ID())
			}
		}
	}
	for _, e := range entries {
		add(typeKey{e.APIVersion(), e.Kind()}, e.ID(), "")
	}

	out := make(map[typeKey]TypeView, len(declared))
	for k, fps := range declared {
		v := TypeView{APIVersion: k.apiVersion, Kind: k.kind, Fingerprints: []FingerprintView{}}
		for fp, ids := range fps {
			fv := FingerprintView{Fingerprint: fp, Agents: make([]string, 0, len(ids))}
			fv.Refused, _ = s.Refusal(fp)
			for id := range ids {
				fv.Agents = append(fv.Agents, id)
			}
			slices.Sort(fv.Agents)
			if e, ok := byFingerprint[fp]; ok {
				fv.Schema = e.Schema()
				fv.FirstSeenAt = e.FirstSeenAt()
			}
			v.Fingerprints = append(v.Fingerprints, fv)
		}
		slices.SortFunc(v.Fingerprints, func(a, b FingerprintView) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
		out[k] = v
	}
	return out, nil
}
