package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// pager is a store with one page of one row per listing. The embedded
// interface is nil, so a helper that reaches for anything else panics.
type pager struct {
	storage.Storage
	listed int
	err    error
}

func (p *pager) list() error { p.listed++; return p.err }

func (p *pager) ListAgents(context.Context, storage.AgentFilter, storage.ListOptions) (*storage.AgentListResult, error) {
	return &storage.AgentListResult{Items: []*model.Agent{{}}}, p.list()
}

func (p *pager) ListRollouts(context.Context, storage.RolloutFilter, storage.ListOptions) (*storage.RolloutListResult, error) {
	return &storage.RolloutListResult{Items: []*model.Rollout{{}}}, p.list()
}

func (p *pager) ListSpecs(context.Context, storage.SpecFilter, storage.ListOptions) (*storage.SpecListResult, error) {
	return &storage.SpecListResult{Items: []*model.Spec{{}}}, p.list()
}

func (p *pager) ListExecutions(context.Context, storage.ExecutionFilter, storage.ListOptions) (*storage.ExecutionListResult, error) {
	return &storage.ExecutionListResult{Items: []*model.ExecutionRecord{{}}}, p.list()
}

// scanner is a pager that also scans, and returns two rows where the pager
// returns one, so that the two paths tell apart.
type scanner struct {
	pager
	scanned int
	filter  any
	err     error
}

func (s *scanner) scan(f any) error { s.scanned++; s.filter = f; return s.err }

func (s *scanner) ScanAgents(_ context.Context, f storage.AgentFilter) ([]*model.Agent, error) {
	return []*model.Agent{{}, {}}, s.scan(f)
}

func (s *scanner) ScanRollouts(_ context.Context, f storage.RolloutFilter) ([]*model.Rollout, error) {
	return []*model.Rollout{{}, {}}, s.scan(f)
}

func (s *scanner) ScanSpecs(_ context.Context, f storage.SpecFilter) ([]*model.Spec, error) {
	return []*model.Spec{{}, {}}, s.scan(f)
}

func (s *scanner) ScanExecutions(_ context.Context, f storage.ExecutionFilter) ([]*model.ExecutionRecord, error) {
	return []*model.ExecutionRecord{{}, {}}, s.scan(f)
}

// helper is one All* helper, run on a store with a filter; it returns the
// number of rows it got.
type helper struct {
	name string
	all  func(context.Context, storage.Storage, any) (int, error)
}

var helpers = []helper{
	{"AllAgents", func(ctx context.Context, s storage.Storage, f any) (int, error) {
		rows, err := storage.AllAgents(ctx, s, f)
		return len(rows), err
	}},
	{"AllRollouts", func(ctx context.Context, s storage.Storage, f any) (int, error) {
		rows, err := storage.AllRollouts(ctx, s, f)
		return len(rows), err
	}},
	{"AllSpecs", func(ctx context.Context, s storage.Storage, f any) (int, error) {
		rows, err := storage.AllSpecs(ctx, s, f)
		return len(rows), err
	}},
	{"AllExecutions", func(ctx context.Context, s storage.Storage, f any) (int, error) {
		rows, err := storage.AllExecutions(ctx, s, f)
		return len(rows), err
	}},
}

func TestAllPagesAStoreThatIsNotAScanner(t *testing.T) {
	t.Parallel()

	for _, h := range helpers {
		t.Run(h.name, func(t *testing.T) {
			p := &pager{}
			rows, err := h.all(context.Background(), p, new(int))
			if err != nil || rows != 1 || p.listed != 1 {
				t.Fatalf("got %d rows, %v after %d listings, want the one row of its one page", rows, err, p.listed)
			}

			failed := errors.New("list failed")
			if _, err := h.all(context.Background(), &pager{err: failed}, nil); !errors.Is(err, failed) {
				t.Fatalf("err = %v, want the error of the listing", err)
			}
		})
	}
}

func TestAllScansAScanner(t *testing.T) {
	t.Parallel()

	for _, h := range helpers {
		t.Run(h.name, func(t *testing.T) {
			filter := new(int) // any pointer: a filter is opaque to the helper, which must hand it on as it is
			s := &scanner{}
			rows, err := h.all(context.Background(), s, filter)
			if err != nil || rows != 2 || s.scanned != 1 || s.listed != 0 {
				t.Fatalf("got %d rows, %v after %d scans and %d listings, want the two rows of one scan and no listing", rows, err, s.scanned, s.listed)
			}
			if s.filter != filter {
				t.Fatalf("the scan got filter %v, want the one the caller passed", s.filter)
			}

			failed := errors.New("scan failed")
			if _, err := h.all(context.Background(), &scanner{err: failed}, nil); !errors.Is(err, failed) {
				t.Fatalf("err = %v, want the error of the scan", err)
			}
		})
	}
}
