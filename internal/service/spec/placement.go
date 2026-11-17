package spec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/workloadschema"
)

// PreviewPlacement evaluates a saved specification without mutating storage or
// contacting agents. Deploy computes the same plan inside its write transaction.
// Inventory can change after preview: Deploy refuses the plan's Token once it no
// longer matches. CheckedAt identifies the observation.
func (s *Service) PreviewPlacement(ctx context.Context, specID string) (*model.PlacementPlan, error) {
	if specID == "" {
		return nil, storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ts, err := s.store.GetSpec(ctx, specID)
	if err != nil {
		return nil, err
	}
	plan, _, err := loadPlacement(ctx, s.store, s.schemas, ts, time.Now())
	return plan, err
}

// loadPlacement reads the inventory and the spec's rollouts, and returns the
// plan with its token and the steps Deploy executes for it. Preview and Deploy
// both call it, so the same stored state gives them the same token.
func loadPlacement(ctx context.Context, store storage.Storage, schemas SchemaSource, ts *model.Spec, now time.Time) (*model.PlacementPlan, []deployStep, error) {
	agents, err := placementAgents(ctx, store, ts)
	if err != nil {
		return nil, nil, err
	}
	rollouts, err := storage.AllRollouts(ctx, store, store.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: ts.ID()}))
	if err != nil {
		return nil, nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, nil, err
	}
	plan, err := buildPlacement(ctx, ts, agents, now, schemas)
	if err != nil {
		return nil, nil, err
	}
	steps := deploySteps(plan, rollouts)
	plan.Token = planToken(plan, steps)
	return plan, steps, nil
}

// placementAgents reads the agents a spec's placement considers: all of them
// when it selects by label, else the explicit targets that exist.
func placementAgents(ctx context.Context, store storage.Storage, ts *model.Spec) ([]*model.Agent, error) {
	var agents []*model.Agent
	if len(ts.TargetLabels()) > 0 {
		var err error
		agents, err = storage.AllAgents(ctx, store, nil)
		if err != nil {
			return nil, err
		}
	} else {
		seen := make(map[string]bool)
		for _, id := range ts.Targets() {
			if seen[id] {
				continue
			}
			seen[id] = true
			a, err := store.GetAgent(ctx, id)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			agents = append(agents, a)
		}
	}
	return agents, nil
}

// prefetchedSchemas answers Compiled from schemas read before a transaction
// opened. The catalog's own source reads the store, which Deploy's transaction
// holds, so inside the transaction a fingerprint missing here is unknown, not read.
type prefetchedSchemas map[string]*workloadschema.Schema

func (p prefetchedSchemas) Compiled(_ context.Context, fingerprint string) (*workloadschema.Schema, bool, error) {
	schema, ok := p[fingerprint]
	return schema, ok, nil
}

// prefetchSchemas compiles, outside any transaction, every schema the runners of
// the spec's placement publish.
func (s *Service) prefetchSchemas(ctx context.Context, specID string) (prefetchedSchemas, error) {
	out := prefetchedSchemas{}
	if s.schemas == nil {
		return out, nil
	}
	ts, err := s.store.GetSpec(ctx, specID)
	if err != nil {
		return nil, err
	}
	agents, err := placementAgents(ctx, s.store, ts)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		for _, c := range a.Capabilities() {
			for _, gvk := range c.Workloads {
				if gvk.SchemaFingerprint == "" || out[gvk.SchemaFingerprint] != nil {
					continue
				}
				schema, known, err := s.schemas.Compiled(ctx, gvk.SchemaFingerprint)
				if err != nil {
					return nil, err
				}
				if known {
					out[gvk.SchemaFingerprint] = schema
				}
			}
		}
	}
	return out, nil
}

// deployAction is what Deploy does with one agent's rollout of the spec.
type deployAction string

const (
	// deployInstall creates the rollout of a target that has none.
	deployInstall deployAction = "install"
	// deployUpdate brings a target's rollout to the spec generation. It writes
	// nothing when the rollout is already synced at that generation.
	deployUpdate deployAction = "update"
	// deployUninstall queues the removal of a rollout whose agent is no longer a target.
	deployUninstall deployAction = "uninstall"
)

// deployStep is one agent of a plan: whether Deploy targets it, whether a target
// exists in discovery and is eligible, the runner the plan selects for it and
// the action on its rollout (nil for an install).
type deployStep struct {
	agentID  string
	targeted bool
	exists   bool
	eligible bool
	runner   string
	action   deployAction
	rollout  *model.Rollout
}

// deploySteps maps a plan onto the spec's existing rollouts, sorted by agent ID.
// Deploy executes these steps and the plan token covers their targets, so
// preview and Deploy derive each agent's action from the same rollout rows in
// one place. An action depends only on whether a rollout exists: sync progress
// (status, task ID, observed generation) changes neither the action nor the token.
func deploySteps(plan *model.PlacementPlan, rollouts []*model.Rollout) []deployStep {
	existing := make(map[string]*model.Rollout, len(rollouts))
	for _, r := range rollouts {
		if r != nil {
			existing[r.AgentID()] = r
		}
	}
	steps := make([]deployStep, 0, len(plan.Targets)+len(existing))
	for _, target := range plan.Targets {
		step := deployStep{
			agentID: target.AgentID, targeted: true, exists: target.Exists, eligible: target.Eligible,
			runner: target.SelectedRunner, action: deployInstall,
		}
		if r, ok := existing[target.AgentID]; ok {
			step.action, step.rollout = deployUpdate, r
			delete(existing, target.AgentID)
		}
		steps = append(steps, step)
	}
	for agentID, r := range existing {
		steps = append(steps, deployStep{agentID: agentID, action: deployUninstall, rollout: r})
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].agentID < steps[j].agentID })
	return steps
}

// planToken is the lowercase hex of the first 16 bytes of SHA-256 over the spec
// ID, version and generation and, for each target in agent ID order, the agent
// ID, whether it exists and is eligible, the runner and the action. Strings are
// length prefixed, so two different plans never share an encoding.
//
// Steps that only uninstall are left out: the target set already changes when
// an agent leaves the plan, and the sync runner deleting such a rollout once its
// uninstall completes must not ask for a new review. CheckedAt and the heartbeat
// observations (status, last seen, heartbeat due) are left out too: a heartbeat
// alone never changes the token.
func planToken(plan *model.PlacementPlan, steps []deployStep) string {
	b := appendTokenString(nil, plan.SpecID)
	b = binary.AppendVarint(b, int64(plan.SpecVersion))
	b = binary.AppendVarint(b, int64(plan.Generation))
	for _, step := range steps {
		if !step.targeted {
			continue
		}
		b = appendTokenString(b, step.agentID)
		var flags byte
		if step.exists {
			flags |= 1
		}
		if step.eligible {
			flags |= 2
		}
		b = append(b, flags)
		b = appendTokenString(b, step.runner)
		b = appendTokenString(b, string(step.action))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

func appendTokenString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// buildPlacement is the single policy for preview and actual deployment. It
// reads only the catalog behind schemas, to check the body against the schemas
// the runners publish; a nil source knows no schema.
// Explicit IDs are unioned with ALL-label matches. Empty labels match no extra
// agents. Matching runners retain registration order, not alphabetical order.
func buildPlacement(ctx context.Context, ts *model.Spec, agents []*model.Agent, now time.Time, schemas SchemaSource) (*model.PlacementPlan, error) {
	plan := &model.PlacementPlan{
		SpecID: ts.ID(), SpecVersion: ts.Version(), Generation: ts.Generation(), CheckedAt: now,
		DeployAllowed: !ts.DeletionRequested(), DeletionRequested: ts.DeletionRequested(), Targets: []model.PlacementTarget{},
	}
	if ts.DeletionRequested() {
		plan.BlockingReason = "spec_deleting"
	}
	inventory := make(map[string]*model.Agent, len(agents))
	for _, a := range agents {
		if a != nil {
			inventory[a.ID()] = a
		}
	}
	wanted := make(map[string][]string)
	for _, id := range ts.Targets() {
		if id == "" {
			return nil, storage.ErrInvalidArgument
		}
		wanted[id] = []string{"explicit"}
	}
	labels := ts.TargetLabels()
	if len(labels) > 0 {
		for id, a := range inventory {
			if agentMatchesLabels(a, labels) {
				wanted[id] = append(wanted[id], "labels")
			}
		}
	}
	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	plan.NoTargets = len(ids) == 0
	cmp := &schemaCompat{ctx: ctx, schemas: schemas, workload: ts.Workload(), memo: make(map[string]compat)}
	for _, id := range ids {
		target, err := planTarget(inventory[id], ts, now, cmp)
		if err != nil {
			return nil, err
		}
		target.AgentID = id
		target.MatchedBy = wanted[id]
		plan.Targets = append(plan.Targets, target)
		if !target.Eligible {
			plan.DeployAllowed = false
		}
	}
	if !ts.DeletionRequested() {
		if missing, _, _ := blockedTargets(plan); len(missing) > 0 {
			plan.BlockingReason = "unknown_targets"
		} else if _, unsupported, _ := blockedTargets(plan); len(unsupported) > 0 {
			plan.BlockingReason = "unsupported_targets"
		} else if _, _, incompatible := blockedTargets(plan); len(incompatible) > 0 {
			plan.BlockingReason = "incompatible_targets"
		}
	}
	return plan, nil
}

func agentMatchesLabels(a *model.Agent, labels map[string]string) bool {
	for k, v := range labels {
		if actual, ok := a.Label(k); !ok || actual != v {
			return false
		}
	}
	return true
}

// compat is how a runner's schema relates to the spec's workload body.
type compat struct{ reason, detail string }

// schemaCompat decides, for a runner fingerprint, whether the spec's body fits.
// Each fingerprint is checked once per plan.
type schemaCompat struct {
	ctx      context.Context
	schemas  SchemaSource
	workload model.Workload
	memo     map[string]compat
}

// check gives the placement reason for a runner that publishes fingerprint:
//   - empty                          schema_unpublished
//   - the spec's own fingerprint     compatible
//   - not in the catalog (yet)       schema_pending
//   - known and the body is valid    compatible
//   - known and the body is invalid  schema_incompatible, or too expensive to validate
func (c *schemaCompat) check(fingerprint string) (compat, error) {
	if fingerprint == "" {
		return compat{reason: "schema_unpublished"}, nil
	}
	if fingerprint == c.workload.SchemaFingerprint {
		return compat{reason: "compatible"}, nil
	}
	if got, ok := c.memo[fingerprint]; ok {
		return got, nil
	}
	got := compat{reason: "schema_pending"}
	if c.schemas != nil {
		schema, known, err := c.schemas.Compiled(c.ctx, fingerprint)
		if err != nil {
			return compat{}, err
		}
		if known {
			body := c.workload.Body
			if body == nil {
				body = map[string]any{}
			}
			var verr *workloadschema.ValidationError
			switch err = schema.Validate(body); {
			case err == nil:
				got = compat{reason: "compatible"}
			case errors.Is(err, workloadschema.ErrValidationTooExpensive):
				got = compat{reason: "schema_incompatible", detail: "the body was too expensive to validate against this schema"}
			case errors.As(err, &verr):
				got = compat{reason: "schema_incompatible", detail: problemsText(verr)}
			default:
				got = compat{reason: "schema_incompatible", detail: err.Error()}
			}
		}
	}
	c.memo[fingerprint] = got
	return got, nil
}

// problemsText lists the first problems of a refusal, as the service does when it refuses a body.
func problemsText(verr *workloadschema.ValidationError) string {
	problems := verr.Problems
	if len(problems) > maxProblems {
		problems = problems[:maxProblems]
	}
	parts := make([]string, len(problems))
	for i, p := range problems {
		parts[i] = p.Path + ": " + p.Message
		if p.Path == "" {
			parts[i] = p.Message
		}
	}
	return strings.Join(parts, "; ")
}

func planTarget(a *model.Agent, ts *model.Spec, now time.Time, cmp *schemaCompat) (model.PlacementTarget, error) {
	t := model.PlacementTarget{Status: "unknown", HeartbeatFreshness: "unknown", RunnerNames: []string{}, Runners: []model.PlacementRunner{}}
	if a == nil {
		t.EligibilityReason = "missing_agent"
		return t, nil
	}
	t.Exists = true
	t.AgentName = a.Name()
	t.Status = a.Status().String()
	t.LastSeenAt = a.LastSeenAt()
	t.HeartbeatDueAt = a.StaleAt()
	if !t.LastSeenAt.IsZero() && !t.HeartbeatDueAt.IsZero() {
		t.HeartbeatFreshness = "fresh"
		if now.After(t.HeartbeatDueAt) {
			t.HeartbeatFreshness = "overdue"
		}
	}
	capabilities := a.Capabilities()
	t.InventoryKnown = len(capabilities) > 0
	if !t.InventoryKnown {
		t.Eligible = true
		t.EligibilityReason = "inventory_unknown"
		return t, nil
	}
	// The workload type is matched exactly. A spec without a workload has no
	// type, and matches no runner.
	want, wantErr := cmp.workload.GVK()
	anyWorkload, anyLabels := false, false
	firstSchemaReason := ""
	selector := ts.RunnerLabels()
	for _, cap := range capabilities {
		r := model.PlacementRunner{Name: cap.Name, Labels: cap.Labels, LabelsMatch: true}
		var fingerprint string
		if wantErr == nil {
			for _, gvk := range cap.Workloads {
				if gvk.Group == want.Group && gvk.Version == want.Version && gvk.Kind == want.Kind {
					r.WorkloadMatch = true
					fingerprint = gvk.SchemaFingerprint
					anyWorkload = true
					break
				}
			}
		}
		for k, v := range selector {
			if actual, ok := cap.Labels[k]; !ok || actual != v {
				r.LabelsMatch = false
				break
			}
		}
		switch {
		case !r.WorkloadMatch:
			r.Reason = "workload_unsupported"
		case !r.LabelsMatch:
			r.Reason = "runner_labels_mismatch"
		default:
			anyLabels = true
			c, err := cmp.check(fingerprint)
			if err != nil {
				return t, err
			}
			r.Reason, r.Detail = c.reason, c.detail
			if firstSchemaReason == "" {
				firstSchemaReason = c.reason
			}
		}
		r.Eligible = r.Reason == "compatible"
		t.Runners = append(t.Runners, r)
		if r.Eligible {
			t.RunnerNames = append(t.RunnerNames, r.Name)
		}
	}
	switch {
	case len(t.RunnerNames) > 0:
		t.Eligible = true
		t.SelectedRunner = t.RunnerNames[0]
		t.EligibilityReason = "compatible_runner"
	case anyLabels:
		t.EligibilityReason = firstSchemaReason
	case anyWorkload:
		t.EligibilityReason = "runner_labels_mismatch"
	default:
		t.EligibilityReason = "workload_unsupported"
	}
	return t, nil
}

// blockedTargets sorts the targets that stop a deploy: agents missing from
// discovery, agents with no runner for the workload or its labels, and agents
// whose runners publish no schema the body fits.
func blockedTargets(plan *model.PlacementPlan) (missing, unsupported, incompatible []string) {
	for _, target := range plan.Targets {
		switch {
		case !target.Exists:
			missing = append(missing, target.AgentID)
		case target.Eligible:
		case isSchemaReason(target.EligibilityReason):
			incompatible = append(incompatible, target.AgentID)
		default:
			unsupported = append(unsupported, target.AgentID)
		}
	}
	return missing, unsupported, incompatible
}

func isSchemaReason(reason string) bool {
	switch reason {
	case "schema_incompatible", "schema_pending", "schema_unpublished":
		return true
	}
	return false
}
