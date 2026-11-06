package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
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

// refusal is the error of a request that podium cannot carry out in the state it
// is in, whatever the request says. errkind classifies it as FailedPrecondition,
// which handlers answer with 409.
type refusal string

func (e refusal) Error() string { return string(e) }

// ErrorKind classifies the error for the transports (errkind).
func (refusal) ErrorKind() errkind.Kind { return errkind.FailedPrecondition }

var (
	// ErrAuthDisabled refuses ResetCredential while agent authentication is off
	// (agent_auth.require false): podium checks and presents no token, so there is
	// no credential to reset.
	ErrAuthDisabled = refusal("agent: agent authentication is off, so there is no credential to reset")

	// ErrGenerationExhausted refuses ResetCredential for a derived credential at
	// the last generation: the next one would wrap around to generation 0, the
	// token its agent ID enrolls with, which podium agent-token printed before
	// there were generations.
	ErrGenerationExhausted = refusal("agent: the credential is at the last generation and cannot be reset")
)

// DerivedCredentialError says that podium cannot use the derived credential of
// an agent: no enrollment secret is set (NoSecret), or the credential derives
// from another secret than the current one. It is an ErrUnauthenticated, so
// discovery refuses the agent's syncs with it. AgentToken returns it too, so
// podium makes no call to the agent; errkind classifies it as
// FailedPrecondition, not as the caller's own failed authentication. Its text
// names the agent and what it needs, never a token.
type DerivedCredentialError struct {
	AgentID    string
	Generation uint32
	NoSecret   bool
}

func (e *DerivedCredentialError) Error() string {
	if e.NoSecret {
		return fmt.Sprintf("agent %s holds a credential derived from an enrollment secret, and no enrollment secret is set: it needs the secret it enrolled under", e.AgentID)
	}
	return fmt.Sprintf("agent %s enrolled under another enrollment secret: it needs the token that podium agent-token --generation %d %s prints with the current secret", e.AgentID, e.Generation, e.AgentID)
}

// Unwrap makes the error an ErrUnauthenticated.
func (e *DerivedCredentialError) Unwrap() error { return ErrUnauthenticated }

// ErrorKind classifies the error for the transports (errkind).
func (e *DerivedCredentialError) ErrorKind() errkind.Kind { return errkind.FailedPrecondition }

// Service provides agent management operations.
type Service struct {
	logger       zerolog.Logger
	store        storage.Storage
	enrollSecret string
	secretID     []byte // SecretID of enrollSecret, nil without one
	authDisabled bool

	// clock replaces time.Now in tests; nil means time.Now.
	clock func() time.Time
}

// Option configures a Service.
type Option func(*Service)

// MinEnrollSecretBytes is the shortest enrollment secret podium accepts.
const MinEnrollSecretBytes = 32

// secretIDLabel is the message whose HMAC under a secret is its ID. It holds a
// colon, which no agent ID holds (model.ValidateAgentID), and no NUL byte, which
// the message of every token after generation 0 holds, so it is the message of
// no token, and a secret ID is no part of any agent's token.
const secretIDLabel = "podium:agent-secret-id:v1"

// DeriveToken returns the token of agentID at generation while secret is the
// enrollment secret: the lowercase hex of HMAC-SHA256 under the secret of the
// agent ID at generation 0, and of the agent ID, a NUL byte and the generation
// in decimal at a generation of 1 or more. Generation 0 is the token an agent
// ID enrolls with, which podium agent-token printed before there were
// generations, so those tokens stay valid. `podium agent-token` prints it.
func DeriveToken(secret, agentID string, generation uint32) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(agentID))
	if generation > 0 {
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(strconv.FormatUint(uint64(generation), 10)))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// SecretID returns the ID of an enrollment secret, which a derived credential
// keeps to name the secret it derives from: the first 8 bytes of HMAC-SHA256 of
// secretIDLabel under the secret. It tells two secrets apart, and no token
// follows from it; like a token, it would confirm a guess of a weak secret.
func SecretID(secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(secretIDLabel))
	return mac.Sum(nil)[:8]
}

// WithEnrollSecret makes secret the enrollment secret: an unknown agent ID then
// enrolls only with the token DeriveToken derives from secret for that ID at
// generation 0, and gets a derived credential, which stores no token. Derived
// credentials need the secret they derive from. An empty secret leaves
// enrollment to trust on first use, with stored credentials.
func WithEnrollSecret(secret string) Option {
	return func(s *Service) {
		s.enrollSecret = secret
		s.secretID = nil
		if secret != "" {
			s.secretID = SecretID(secret)
		}
	}
}

// WithAuthDisabled tells the service that agent auth is off
// (agent_auth.require false): podium checks no token and presents none, so
// AgentToken looks none up and returns "".
func WithAuthDisabled() Option {
	return func(s *Service) { s.authDisabled = true }
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
// First contact (no credential) refuses a token longer than
// model.AgentTokenMaxBytes (ErrTokenTooLong). With an enrollment secret, it
// enrolls only the token DeriveToken derives for the agent ID at generation 0
// (constant-time compare), as a derived credential, which stores no token; any
// other returns ErrEnrollRefused, an ErrUnauthenticated, and stores nothing.
// Without a secret, it enrolls the presented token as a stored credential.
//
// A later sync is checked against the credential:
//   - a stored one is compared with the presented token (constant-time),
//     whatever the length of the stored one and whether a secret is set. With
//     the secret set, a stored token that DeriveToken derives for the agent ID
//     at generation 0 becomes a derived credential;
//   - a derived one needs the secret: the presented token must be the one
//     DeriveToken derives for its generation (constant-time). One of another
//     secret, which the agent got when it was re-provisioned under the current
//     secret, moves the credential to the current secret, at the same
//     generation. Without a secret, or with the token of another secret, the
//     sync is refused with a DerivedCredentialError.
//
// A missing or wrong token returns ErrUnauthenticated. The read-check-write
// runs inside one transaction so two concurrent first-contacts can't both
// enroll and race, and a credential that changes kind or secret changes in the
// transaction that checks it.
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
		case err == nil && existing.Derived():
			return s.verifyDerived(ctx, tx, existing, presented)
		case err == nil:
			return s.verifyStored(ctx, tx, existing, presented)
		case errors.Is(err, storage.ErrNotFound):
			return s.enroll(ctx, tx, agentID, presented)
		default:
			return err
		}
	})
}

// enroll enrolls an agent ID that has no credential, as VerifyOrEnrollToken says.
func (s *Service) enroll(ctx context.Context, tx storage.Storage, agentID, presented string) error {
	if len(presented) > model.AgentTokenMaxBytes {
		return ErrTokenTooLong
	}
	if s.enrollSecret == "" {
		cred, err := model.NewStoredAgentCredential(agentID, presented)
		if err != nil {
			return err
		}
		return tx.UpsertAgentCredential(ctx, cred)
	}
	if !hmac.Equal([]byte(presented), []byte(DeriveToken(s.enrollSecret, agentID, 0))) {
		return ErrEnrollRefused
	}
	cred, err := model.NewDerivedAgentCredential(agentID, 0, s.secretID)
	if err != nil {
		return err
	}
	return tx.UpsertAgentCredential(ctx, cred)
}

// verifyStored checks presented against a stored credential, and with the secret
// set turns a stored token that the secret derives at generation 0 into a derived
// credential.
func (s *Service) verifyStored(ctx context.Context, tx storage.Storage, existing *model.AgentCredential, presented string) error {
	if subtle.ConstantTimeCompare([]byte(existing.Token()), []byte(presented)) != 1 {
		return ErrUnauthenticated
	}
	if !s.derivesAtZero(existing) {
		return nil
	}
	return s.rederive(ctx, tx, existing, 0)
}

// verifyDerived checks presented against a derived credential, and moves one that
// the token of the current secret re-provisioned to that secret.
func (s *Service) verifyDerived(ctx context.Context, tx storage.Storage, existing *model.AgentCredential, presented string) error {
	if s.enrollSecret == "" {
		return &DerivedCredentialError{AgentID: existing.AgentID(), Generation: existing.Generation(), NoSecret: true}
	}
	current := bytes.Equal(existing.SecretID(), s.secretID)
	if !hmac.Equal([]byte(presented), []byte(DeriveToken(s.enrollSecret, existing.AgentID(), existing.Generation()))) {
		if !current {
			return &DerivedCredentialError{AgentID: existing.AgentID(), Generation: existing.Generation()}
		}
		return ErrUnauthenticated
	}
	if current {
		return nil
	}
	return s.rederive(ctx, tx, existing, existing.Generation())
}

// derivesAtZero reports whether c is a stored credential whose token the
// enrollment secret derives for its agent ID at generation 0.
func (s *Service) derivesAtZero(c *model.AgentCredential) bool {
	return s.enrollSecret != "" && !c.Derived() &&
		hmac.Equal([]byte(c.Token()), []byte(DeriveToken(s.enrollSecret, c.AgentID(), 0)))
}

// rederive replaces the credential c with a derived one of the current secret at
// generation, which keeps c's enrollment time.
func (s *Service) rederive(ctx context.Context, tx storage.Storage, c *model.AgentCredential, generation uint32) error {
	cred, err := model.NewDerivedAgentCredential(c.AgentID(), generation, s.secretID)
	if err != nil {
		return err
	}
	cred.SetCreatedAt(c.CreatedAt())
	return tx.UpsertAgentCredential(ctx, cred)
}

// AgentToken returns the bearer token podium presents to this agent: the token
// of a stored credential, the token DeriveToken derives for the generation of a
// derived one, or "" if the agent is not enrolled. A missing credential is not
// an error. A derived credential with no secret set, or of another secret,
// returns a DerivedCredentialError, so the caller makes no call. It writes
// nothing. With agent auth disabled it returns "" at once: podium presents no
// token.
func (s *Service) AgentToken(ctx context.Context, agentID string) (string, error) {
	if agentID == "" {
		return "", storage.ErrInvalidArgument
	}
	if s.authDisabled {
		return "", nil
	}
	c, err := s.store.GetAgentCredential(ctx, agentID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	if !c.Derived() {
		return c.Token(), nil
	}
	if s.enrollSecret == "" {
		return "", &DerivedCredentialError{AgentID: agentID, Generation: c.Generation(), NoSecret: true}
	}
	if !bytes.Equal(c.SecretID(), s.secretID) {
		return "", &DerivedCredentialError{AgentID: agentID, Generation: c.Generation()}
	}
	return DeriveToken(s.enrollSecret, agentID, c.Generation()), nil
}

// CredentialSweep is what SweepCredentials found.
type CredentialSweep struct {
	// Converted is the stored credentials it made derived: their token was the
	// generation-0 token of their agent ID under the enrollment secret.
	Converted int
	// Stored is the stored credentials left: tokens agents chose while no
	// enrollment secret was set, and tokens an older version stored that the
	// current secret does not derive, an earlier secret's ones among them.
	Stored int
	// Unusable is the derived credentials podium cannot use: all of them while
	// no secret is set, otherwise those of another secret.
	Unusable int
}

// SweepCredentials converts every stored credential whose token the enrollment
// secret derives for its agent ID at generation 0 into a derived credential,
// which keeps its enrollment time, and counts what is left (CredentialSweep).
// It lists the credentials, then converts in one transaction each one that is
// still such a stored credential when the transaction reads it again; one that
// a sync converted or changed in between is left as it is. Without a secret it
// converts nothing, and a sweep with nothing to convert writes nothing.
func (s *Service) SweepCredentials(ctx context.Context) (CredentialSweep, error) {
	creds, err := storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.AgentCredentialListResult, error) {
		return s.store.ListAgentCredentials(ctx, o)
	})
	if err != nil {
		return CredentialSweep{}, err
	}
	var (
		res       CredentialSweep
		derivable []string
	)
	for _, c := range creds {
		switch {
		case c.Derived():
			if s.enrollSecret == "" || !bytes.Equal(c.SecretID(), s.secretID) {
				res.Unusable++
			}
		case s.derivesAtZero(c):
			derivable = append(derivable, c.AgentID())
		default:
			res.Stored++
		}
	}
	if len(derivable) == 0 {
		return res, nil
	}
	err = s.store.WithTx(ctx, func(tx storage.Storage) error {
		res.Converted = 0
		for _, id := range derivable {
			c, err := tx.GetAgentCredential(ctx, id)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if !s.derivesAtZero(c) {
				continue
			}
			if err := s.rederive(ctx, tx, c, 0); err != nil {
				return err
			}
			res.Converted++
		}
		return nil
	})
	if err != nil {
		return CredentialSweep{}, err
	}
	return res, nil
}

// Upsert an agent. The row keeps its creation time, the labels that the control
// plane owns and, when the sync reports none, its heartbeat interval.
//
// The result says what the sync changed in the row: see UpsertResult. It is
// judged inside the transaction, against the row the sync replaces, and is zero
// when the sync fails. A reconnected agent also gets its failed rollouts back
// to pending.
func (s *Service) Upsert(ctx context.Context, m *model.Agent) (UpsertResult, error) {
	if m == nil {
		return UpsertResult{}, storage.ErrInvalidArgument
	}

	var (
		res     UpsertResult
		existed bool
	)
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		res, existed = UpsertResult{}, false
		existing, err := tx.GetAgent(ctx, m.ID())
		switch {
		case err == nil:
			existed = true
			// m comes from a sync, so it is active: a reconnect is an
			// existing row that was not, or an uptime that went down.
			res.Reconnected = m.ReconnectedSince(existing.Status(), existing.UptimeSeconds())
			m.SetCreatedAt(existing.CreatedAt())
			for k, v := range existing.LabelsAll() {
				m.LabelAdd(k, v)
			}
			if m.HeartbeatInterval() == 0 && existing.HeartbeatInterval() > 0 {
				m.SetHeartbeatInterval(existing.HeartbeatInterval())
			}
			res.Visible = m.ChangedVisiblySince(existing)
		case errors.Is(err, storage.ErrNotFound):
			res = UpsertResult{Visible: true, Reconnected: true}
		default:
			return err
		}
		if hb := m.HeartbeatInterval(); hb > 0 {
			m.SetStaleAt(m.LastSeenAt().Add(hb))
		}
		if err := tx.UpsertAgent(ctx, m); err != nil {
			return err
		}
		if res.Reconnected {
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
		return UpsertResult{}, err
	}

	s.logger.Debug().
		Str("agent_id", m.ID()).
		Bool("existed", existed).
		Bool("visible", res.Visible).
		Bool("reconnected", res.Reconnected).
		Str("heartbeat", m.HeartbeatInterval().String()).
		Msg("agent upserted")
	return res, nil
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

// resetTokenBytes is the size of the random token a reset issues while no
// enrollment secret is set: 32 bytes, as 64 hex digits.
const resetTokenBytes = 32

// now is the time of the service's clock.
func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// ResetCredential issues the agent a new credential in place of the one it
// holds, in one transaction: from its commit discovery refuses the old token,
// and AgentToken returns the new one, so the old token stops working in both
// directions at once. The credential keeps its enrollment time.
//
// With an enrollment secret set, the new credential is derived, and names the
// current secret, whatever secret the old one derived from. Its generation is
// that of a derived credential it replaces plus one, and for a stored one, which
// has no generation, the time of the reset (generationFloor).
// IssuedCredential.Generation says which `podium agent-token --generation`
// prints the token again. Without a secret it is a stored credential with a new
// random token, since trust on first use stores the token an agent holds;
// nothing derives that token again, and the credential holds it like any stored
// one.
//
// The token is returned here and in no other result: a caller must neither log
// it nor keep it. An agent with no credential is storage.ErrNotFound, and
// nothing is written. With agent authentication disabled a reset is
// ErrAuthDisabled, and a derived credential at the last generation
// ErrGenerationExhausted, both of which change nothing.
func (s *Service) ResetCredential(ctx context.Context, agentID string) (IssuedCredential, error) {
	if agentID == "" {
		return IssuedCredential{}, storage.ErrInvalidArgument
	}
	if s.authDisabled {
		return IssuedCredential{}, ErrAuthDisabled
	}

	var issued IssuedCredential
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		old, err := tx.GetAgentCredential(ctx, agentID)
		if err != nil {
			return err
		}
		if s.enrollSecret != "" {
			generation, err := s.nextGeneration(old)
			if err != nil {
				return err
			}
			issued = IssuedCredential{AgentID: agentID, Token: DeriveToken(s.enrollSecret, agentID, generation), Derived: true, Generation: generation}
			return s.rederive(ctx, tx, old, generation)
		}
		var raw [resetTokenBytes]byte
		if _, err = rand.Read(raw[:]); err != nil {
			return err
		}
		token := hex.EncodeToString(raw[:])
		cred, err := model.NewStoredAgentCredential(agentID, token)
		if err != nil {
			return err
		}
		cred.SetCreatedAt(old.CreatedAt())
		issued = IssuedCredential{AgentID: agentID, Token: token}
		return tx.UpsertAgentCredential(ctx, cred)
	})
	if err != nil {
		return IssuedCredential{}, err
	}
	return issued, nil
}

// nextGeneration is the generation a reset with the secret set gives in place of
// the credential c: the one after c's for a derived credential, or
// ErrGenerationExhausted at the last one, which would wrap around to generation
// 0, and generationFloor for a stored credential, which has none.
func (s *Service) nextGeneration(c *model.AgentCredential) (uint32, error) {
	if !c.Derived() {
		return generationFloor(s.now()), nil
	}
	if c.Generation() == math.MaxUint32 {
		return 0, ErrGenerationExhausted
	}
	return c.Generation() + 1, nil
}

// generationFloor is the generation a reset with the secret set gives a stored
// credential at the time now: now in Unix seconds, 1 for a time at or before the
// epoch, and math.MaxUint32-1 at most, so that ErrGenerationExhausted stays the
// guard of the last generation. Counting a stored credential as generation 0
// would issue generation 1, whose token a reset may have revoked: an agent
// derived at generation 2 whose secret was removed, reset to a stored credential,
// and reset again once the same secret is back, would get the token of
// generation 1 again. The floor is above every generation the agent held before
// as long as the clock of the node that serves the reset, the leader, has not run
// back past an earlier floor, and the credential was reset fewer times since that
// floor than seconds have passed: each reset of a derived credential adds one.
func generationFloor(now time.Time) uint32 {
	switch seconds := now.Unix(); {
	case seconds < 1:
		return 1
	case seconds >= math.MaxUint32:
		return math.MaxUint32 - 1
	default:
		return uint32(seconds)
	}
}

// Delete removes the agent's row and every rollout of the agent in one
// transaction, so that an agent never loses its rollouts and keeps its row, or
// the reverse. It returns the row it removed. The credential of the agent stays,
// and so does its execution history: an agent that still runs registers again at
// its next sync, with its credential, and without the labels operators set and
// without rollouts (lifecycle deletion, in contrast, leaves the rollouts).
// ErrNotFound if the agent has no row, whatever else the store holds of it.
func (s *Service) Delete(ctx context.Context, id string) (*model.Agent, error) {
	if id == "" {
		return nil, storage.ErrInvalidArgument
	}

	var removed *model.Agent
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		ag, err := tx.GetAgent(ctx, id)
		if err != nil {
			return err
		}
		rollouts, err := storage.AllRollouts(ctx, tx, tx.BuildRolloutFilter(storage.RolloutQueryCriteria{AgentID: id}))
		if err != nil {
			return err
		}
		for _, ro := range rollouts {
			if err = tx.DeleteRollout(ctx, ro.ID()); err != nil {
				return err
			}
		}
		if err = tx.DeleteAgent(ctx, id); err != nil {
			return err
		}
		removed = ag
		return nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}
