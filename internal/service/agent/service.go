package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// ErrUnauthenticated indicates an agent presented a missing or wrong bearer
// token during discovery. Handlers map it to HTTP 401 / gRPC Unauthenticated.
var ErrUnauthenticated = errors.New("agent: unauthenticated")

// ErrEnrollRefused indicates an unknown agent ID presented a token other than
// the one derived for it from the enrollment secret. It is an
// ErrUnauthenticated, which handlers already map.
var ErrEnrollRefused = fmt.Errorf("%w: not the enrollment token of this agent ID", ErrUnauthenticated)

// ErrTokenTooLong indicates a bearer token over model.AgentTokenMaxBytes,
// which discovery never enrolls. Handlers map it to HTTP 400 / gRPC
// InvalidArgument.
var ErrTokenTooLong = errors.New("agent: token too long")

// Service provides agent management operations.
type Service struct {
	logger       zerolog.Logger
	store        storage.Storage
	enrollSecret string
}

// Option configures a Service.
type Option func(*Service)

// MinEnrollSecretBytes is the shortest enrollment secret podium accepts.
const MinEnrollSecretBytes = 32

// EnrollToken returns the token an unknown agent ID must present to enroll
// while an enrollment secret is set: the lowercase hex of HMAC-SHA256 of the
// agent ID under the secret. `podium agent-token` prints it.
func EnrollToken(secret, agentID string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(agentID))
	return hex.EncodeToString(mac.Sum(nil))
}

// WithEnrollSecret closes enrollment: an unknown agent ID then enrolls only
// with the token EnrollToken derives from secret for that ID. Agents that are
// already enrolled are unaffected. An empty secret leaves enrollment to trust
// on first use.
func WithEnrollSecret(secret string) Option {
	return func(s *Service) { s.enrollSecret = secret }
}

// EnrollSecretSet reports whether an enrollment secret closes enrollment.
func (s *Service) EnrollSecretSet() bool { return s.enrollSecret != "" }

// New creates a new agent service.
func New(store storage.Storage, logger zerolog.Logger, opts ...Option) *Service {
	if store == nil {
		panic("agent.Service: store is nil")
	}
	s := &Service{
		logger: logger.With().Str("service", "agents").Logger(),
		store:  store,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// List returns a page of agents matching the query.
func (s *Service) List(ctx context.Context, q ListQuery) (*Page, error) {
	res, err := s.store.ListAgents(ctx, q.Filter, storage.ListOptions{
		Limit:  service.NormalizeListLimit(q.Limit, defaultListLimit),
		Cursor: q.Cursor,
	})
	if err != nil {
		return nil, err
	}

	return &Page{
		Items:      res.Items,
		NextCursor: res.NextCursor,
	}, nil
}

// Get returns a single agent by ID.
func (s *Service) Get(ctx context.Context, id string) (*model.Agent, error) {
	if id == "" {
		return nil, storage.ErrInvalidArgument
	}
	return s.store.GetAgent(ctx, id)
}

// VerifyOrEnrollToken implements bearer auth for an agent: trust on first use,
// or enrollment by derived token when an enrollment secret is set.
//
// First contact (no stored credential) enrolls the presented token, unless it
// is longer than model.AgentTokenMaxBytes (ErrTokenTooLong). With an
// enrollment secret, first contact enrolls only the token EnrollToken derives
// for the agent ID (constant-time compare); any other returns
// ErrEnrollRefused, an ErrUnauthenticated, and stores nothing. Every later
// sync must present the stored token (constant-time compare), whatever the
// length of the stored one and whether a secret is set; a missing or wrong
// token returns ErrUnauthenticated. The read-check-write runs inside one
// transaction so two concurrent first-contacts can't both enroll and race.
func (s *Service) VerifyOrEnrollToken(ctx context.Context, agentID, presented string) error {
	if agentID == "" {
		return storage.ErrInvalidArgument
	}
	if presented == "" {
		return ErrUnauthenticated
	}

	return s.store.WithTx(ctx, func(tx storage.Storage) error {
		existing, err := tx.GetAgentCredential(ctx, agentID)
		switch {
		case err == nil:
			if subtle.ConstantTimeCompare([]byte(existing.Token()), []byte(presented)) != 1 {
				return ErrUnauthenticated
			}
			return nil
		case errors.Is(err, storage.ErrNotFound):
			if len(presented) > model.AgentTokenMaxBytes {
				return ErrTokenTooLong
			}
			if s.enrollSecret != "" && !hmac.Equal([]byte(presented), []byte(EnrollToken(s.enrollSecret, agentID))) {
				return ErrEnrollRefused
			}
			cred, err := model.NewAgentCredential(agentID, presented)
			if err != nil {
				return err
			}
			return tx.UpsertAgentCredential(ctx, cred)
		default:
			return err
		}
	})
}

// AgentToken returns the bearer token enrolled for this agent, or "" if the
// agent is not enrolled. A missing credential is not an error. The proxy pool
// presents the token only when agent auth is required.
func (s *Service) AgentToken(ctx context.Context, agentID string) (string, error) {
	if agentID == "" {
		return "", storage.ErrInvalidArgument
	}
	c, err := s.store.GetAgentCredential(ctx, agentID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return c.Token(), nil
}

// Upsert an agent.
func (s *Service) Upsert(ctx context.Context, m *model.Agent) error {
	if m == nil {
		return storage.ErrInvalidArgument
	}

	var existed bool
	var reconnected bool
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		existing, err := tx.GetAgent(ctx, m.ID())
		switch {
		case err == nil:
			existed = true
			// m comes from a sync, so it is active: a reconnect is an
			// existing row that was not, or an uptime that went down.
			reconnected = m.ReconnectedSince(existing.Status(), existing.UptimeSeconds())
			m.SetCreatedAt(existing.CreatedAt())
			for k, v := range existing.LabelsAll() {
				m.LabelAdd(k, v)
			}
			if m.HeartbeatInterval() == 0 && existing.HeartbeatInterval() > 0 {
				m.SetHeartbeatInterval(existing.HeartbeatInterval())
			}
		case errors.Is(err, storage.ErrNotFound):
			reconnected = true
		default:
			return err
		}
		if hb := m.HeartbeatInterval(); hb > 0 {
			m.SetStaleAt(m.LastSeenAt().Add(hb))
		}
		if err := tx.UpsertAgent(ctx, m); err != nil {
			return err
		}
		if reconnected {
			rollouts, err := storage.AllRollouts(ctx, tx, tx.BuildRolloutFilter(storage.RolloutQueryCriteria{AgentID: m.ID()}))
			if err != nil {
				return err
			}
			for _, ro := range rollouts {
				if ro.Status() != enum.SyncStatusFailed && ro.Status() != enum.SyncStatusUnknown {
					continue
				}
				if ro.Intent() == enum.RolloutIntentNoop {
					if ro.ActualTaskID() == "" {
						ro.SetIntent(enum.RolloutIntentInstall)
					} else {
						ro.SetIntent(enum.RolloutIntentUpdate)
					}
				}
				ro.MarkPending(ro.DesiredGeneration())
				if err = tx.UpsertRollout(ctx, ro); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	s.logger.Debug().
		Str("agent_id", m.ID()).
		Bool("existed", existed).
		Str("heartbeat", m.HeartbeatInterval().String()).
		Msg("agent upserted")
	return nil
}

// PatchLabels replaces labels for an agent.
func (s *Service) PatchLabels(ctx context.Context, req PatchLabels) (*model.Agent, error) {
	if req.ID == "" {
		return nil, storage.ErrInvalidArgument
	}

	var (
		result  *model.Agent
		changed bool
	)
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		agent, err := tx.GetAgent(ctx, req.ID)
		if err != nil {
			return err
		}

		before := agent.UpdatedAt()
		replaceLabels(agent, req.Labels)
		if !agent.UpdatedAt().Equal(before) {
			if err = tx.UpsertAgent(ctx, agent); err != nil {
				return err
			}
			changed = true
		}
		result = agent.Clone()
		return nil
	})
	if err != nil {
		return nil, err
	}

	if changed {
		s.logger.Debug().
			Str("agent_id", req.ID).
			Int("labels", len(req.Labels)).
			Msg("labels patched")
	}
	return result, nil
}

func replaceLabels(a *model.Agent, labels map[string]string) {
	for k := range a.LabelsAll() {
		if v, ok := labels[k]; !ok || v == "" {
			a.LabelDelete(k)
		}
	}
	for k, v := range labels {
		if k == "" || v == "" {
			continue
		}
		a.LabelAdd(k, v)
	}
}
