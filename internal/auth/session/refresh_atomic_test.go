package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

type countingRefreshIssuer struct {
	calls atomic.Int32
	err   error
}

func (i *countingRefreshIssuer) Issue(context.Context, *identity.Identity) (string, error) {
	count := i.calls.Add(1)
	if i.err != nil {
		return "", i.err
	}
	return fmt.Sprint("test-access-", count), nil
}

type refreshReadBarrier struct {
	storage.Storage
	read    atomic.Int32
	both    chan struct{}
	release chan struct{}
}

func (s *refreshReadBarrier) GetSession(ctx context.Context, id string) (*model.Session, error) {
	value, err := s.Storage.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	count := s.read.Add(1)
	if count == 2 {
		close(s.both)
	}
	if count <= 2 {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return value, nil
}

func TestServiceConcurrentRefreshHasOneRotationWinner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st := inmemory.New()
	now := time.Unix(1790896000, 0)
	user := mustUser(t, "refresh-user", "refresh-subject", false)
	mustUpsertUser(t, ctx, st, user)
	raw, hash, err := newRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	original, err := model.NewSession("review-session", user.ID(), "credential", enum.Password, hash, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, original); err != nil {
		t.Fatal(err)
	}
	wrapped := &refreshReadBarrier{Storage: st, both: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(wrapped.release) }) }
	defer release()
	svc := newService(t, wrapped, &fakeClock{t: now}, &countingRefreshIssuer{}, &fakeRBAC{perms: []enum.Permission{enum.SpecsGet}}, true)
	type result struct {
		pair *TokenPair
		err  error
	}
	replies := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() { pair, _, err := svc.Refresh(ctx, original.ID(), raw); replies <- result{pair, err} }()
	}
	var received []result
	select {
	case <-wrapped.both:
		release()
	case r := <-replies:
		received = append(received, r)
		release()
	case <-ctx.Done():
		t.Fatal("refresh did not reach the deterministic read barrier or finish")
	}
	for len(received) < 2 {
		select {
		case r := <-replies:
			received = append(received, r)
		case <-ctx.Done():
			t.Fatal("refresh callers did not finish")
		}
	}
	winners := 0
	for _, r := range received {
		if r.err == nil {
			winners++
		} else if !errors.Is(r.err, auth.ErrInvalidRefresh) {
			t.Errorf("unexpected loser error:%v", r.err)
		}
	}
	if winners != 1 {
		t.Fatalf("same consumed refresh token yielded%d successful rotations; expected one winner", winners)
	}
}

type refreshGateRBAC struct {
	entered, release chan struct{}
	once             sync.Once
}

func (r *refreshGateRBAC) ResolveUserPermissions(ctx context.Context, _ *model.User) ([]enum.Permission, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
		return []enum.Permission{enum.SpecsGet}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func refreshFixture(t *testing.T) (*inmemory.Store, *model.Session, string, *fakeClock) {
	t.Helper()
	st := inmemory.New()
	ctx := context.Background()
	now := time.Unix(1790896000, 123)
	user := mustUser(t, "refresh-user", "refresh-subject", false)
	mustUpsertUser(t, ctx, st, user)
	raw, hash, err := newRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	original, err := model.NewSession("atomic-refresh", user.ID(), "credential", enum.Password, hash, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	original.SetCreatedAt(now.Add(-time.Hour))
	original.SetUpdatedAt(original.CreatedAt())
	if err := st.CreateSession(ctx, original); err != nil {
		t.Fatal(err)
	}
	return st, original, raw, &fakeClock{t: now}
}

func TestServiceRefreshRechecksCurrentSessionAndUser(t *testing.T) {
	for _, kind := range []string{"revoked", "disabled", "expired"} {
		for _, rotate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rotate-%t", kind, rotate), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				st, original, raw, clock := refreshFixture(t)
				gate := &refreshGateRBAC{entered: make(chan struct{}), release: make(chan struct{})}
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(gate.release) }) }
				defer release()
				issuer := &countingRefreshIssuer{}
				svc := newService(t, st, clock, issuer, gate, rotate)
				replies := make(chan error, 1)
				go func() {
					pair, id, err := svc.Refresh(ctx, original.ID(), raw)
					if pair != nil || id != nil {
						replies <- fmt.Errorf("failed refresh returned tokens")
						return
					}
					replies <- err
				}()
				select {
				case <-gate.entered:
				case <-ctx.Done():
					t.Fatal("refresh did not reach external RBAC")
				}
				want := auth.ErrInvalidRefresh
				switch kind {
				case "revoked":
					want = auth.ErrRevoked
					if err := st.RevokeSession(ctx, original.ID(), clock.Now()); err != nil {
						t.Fatal(err)
					}
				case "disabled":
					user, err := st.GetUser(ctx, original.UserID())
					if err != nil {
						t.Fatal(err)
					}
					user.Disable()
					if err := st.UpsertUser(ctx, user); err != nil {
						t.Fatal(err)
					}
				case "expired":
					clock.Advance(2 * time.Hour)
				}
				release()
				select {
				case err := <-replies:
					if !errors.Is(err, want) {
						t.Fatalf("current %s not enforced:%v", kind, err)
					}
				case <-ctx.Done():
					t.Fatal("refresh did not complete")
				}
				if issuer.calls.Load() != 0 {
					t.Fatal("rejected session reached token issuer")
				}
				got, err := st.GetSession(ctx, original.ID())
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got.RefreshHash(), original.RefreshHash()) || !got.ExpiresAt().Equal(original.ExpiresAt()) {
					t.Fatal("rejected refresh mutated token state")
				}
				if kind == "revoked" && !got.Revoked() {
					t.Fatal("refresh resurrected revoked session")
				}
			})
		}
	}
}

type failedRefreshTransaction struct {
	storage.Storage
	failure error
}

func (s *failedRefreshTransaction) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	return s.Storage.WithTx(ctx, func(tx storage.Storage) error {
		if err := fn(tx); err != nil {
			return err
		}
		return s.failure
	})
}

func TestServiceRefreshFailuresDoNotConsumeToken(t *testing.T) {
	for _, kind := range []string{"issuer", "transaction"} {
		t.Run(kind, func(t *testing.T) {
			st, original, raw, clock := refreshFixture(t)
			sentinel := errors.New("test-refresh-failure")
			issuer := &countingRefreshIssuer{}
			var store storage.Storage = st
			if kind == "issuer" {
				issuer.err = sentinel
			} else {
				store = &failedRefreshTransaction{Storage: st, failure: sentinel}
			}
			svc := newService(t, store, clock, issuer, &fakeRBAC{perms: []enum.Permission{enum.SpecsGet}}, true)
			pair, id, err := svc.Refresh(context.Background(), original.ID(), raw)
			if !errors.Is(err, sentinel) || pair != nil || id != nil {
				t.Fatalf("failure exposed result:pair=%v id=%v err=%v", pair, id, err)
			}
			got, err := st.GetSession(context.Background(), original.ID())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.RefreshHash(), original.RefreshHash()) || !got.ExpiresAt().Equal(original.ExpiresAt()) || !got.UpdatedAt().Equal(original.UpdatedAt()) {
				t.Fatal("failed refresh consumed old token or published partial rotation")
			}
		})
	}
}

func TestServiceRefreshNoRotationRemainsReusable(t *testing.T) {
	st, original, raw, clock := refreshFixture(t)
	counter := &countingRefreshTransactions{Storage: st}
	svc := newService(t, counter, clock, &countingRefreshIssuer{}, &fakeRBAC{perms: []enum.Permission{enum.SpecsGet}}, false)
	for i := 0; i < 2; i++ {
		pair, _, err := svc.Refresh(context.Background(), original.ID(), raw)
		if err != nil || pair == nil || pair.RefreshToken != raw {
			t.Fatalf("nonrotating refresh changed behavior:%v", err)
		}
	}
	if counter.calls.Load() != 0 {
		t.Fatalf("read-only refresh entered%d write transactions", counter.calls.Load())
	}
	got, err := st.GetSession(context.Background(), original.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.RefreshHash(), original.RefreshHash()) || !got.ExpiresAt().Equal(original.ExpiresAt()) || !got.UpdatedAt().Equal(original.UpdatedAt()) {
		t.Fatal("nonrotating refresh mutated session")
	}
}

type countingRefreshTransactions struct {
	storage.Storage
	calls atomic.Int32
}

func (s *countingRefreshTransactions) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	s.calls.Add(1)
	return s.Storage.WithTx(ctx, fn)
}
