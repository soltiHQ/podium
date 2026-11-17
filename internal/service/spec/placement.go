package spec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service/workload"
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
	plan, _, err := loadPlacement(ctx, s.store, s.schemas, s.logger, ts, time.Now(), nil)
	return plan, err
}

// loadPlacement reads the inventory and the spec's rollouts, and returns the
// plan with its token and the steps Deploy executes for it. Preview and Deploy
// both call it, so the same stored state gives them the same token.
//
// The body is validated against the runners' schemas outside any transaction:
// with pre set (Deploy's prefetch) loadPlacement validates nothing and compiles
// nothing, and answers [ErrPlacementChanged] when the spec's workload or runner
// labels are no longer the ones the outcomes were computed for (the labels pick
// the runner each agent routes the workload to); with pre nil it computes them
// from schemas, and logger takes a schema that cannot be loaded.
func loadPlacement(ctx context.Context, store storage.Storage, schemas SchemaSource, logger zerolog.Logger, ts *model.Spec, now time.Time, pre *prefetched) (*model.PlacementPlan, []deployStep, error) {
	agents, err := placementAgents(ctx, store, ts)
	if err != nil {
		return nil, nil, err
	}
	var outcomes compatOutcomes
	if pre != nil {
		if !ts.Workload().Equal(pre.workload) || !maps.Equal(ts.RunnerLabels(), pre.runnerLabels) {
			return nil, nil, ErrPlacementChanged
		}
		outcomes = pre.outcomes
	} else {
		outcomes = outcomesFor(ctx, schemas, logger, ts, agents)
	}
	rollouts, err := storage.AllRollouts(ctx, store, store.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: ts.ID()}))
	if err != nil {
		return nil, nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, nil, err
	}
	plan, err := buildPlacement(ts, agents, now, outcomes)
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

// prefetched is what Deploy computes before its transaction opens: the workload
// and runner labels it read, and for them the outcome of the schema of each
// runner the agents route the workload to. Reading the catalog and validating a
// body inside the transaction would hold the store while they run.
type prefetched struct {
	workload     model.Workload
	runnerLabels map[string]string
	outcomes     compatOutcomes
}

// prefetch reads the spec and its placement's agents and validates the spec's
// workload against the schemas their runners publish, outside any transaction.
func (s *Service) prefetch(ctx context.Context, specID string) (*prefetched, error) {
	ts, err := s.store.GetSpec(ctx, specID)
	if err != nil {
		return nil, err
	}
	agents, err := placementAgents(ctx, s.store, ts)
	if err != nil {
		return nil, err
	}
	return &prefetched{workload: ts.Workload(), runnerLabels: ts.RunnerLabels(), outcomes: outcomesFor(ctx, s.schemas, s.logger, ts, agents)}, nil
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

// buildPlacement is the single pure policy for preview and actual deployment.
// outcomes says, per runner fingerprint, whether the body fits the schema it
// names; a fingerprint without an outcome is pending.
// Explicit IDs are unioned with ALL-label matches. Empty labels match no extra
// agents. Runners retain registration order, not alphabetical order, and each
// target is evaluated on the runner its agent routes the workload to.
func buildPlacement(ts *model.Spec, agents []*model.Agent, now time.Time, outcomes compatOutcomes) (*model.PlacementPlan, error) {
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
	cmp := &schemaCompat{workload: ts.Workload(), outcomes: outcomes}
	for _, id := range ids {
		target := planTarget(inventory[id], ts, now, cmp)
		target.AgentID = id
		target.MatchedBy = wanted[id]
		plan.Targets = append(plan.Targets, target)
		if !target.Eligible {
			plan.DeployAllowed = false
		}
	}
	if !ts.DeletionRequested() {
		switch b := blockedTargets(plan); {
		case len(b.missing) > 0:
			plan.BlockingReason = "unknown_targets"
		case len(b.unsupported) > 0:
			plan.BlockingReason = "unsupported_targets"
		case len(b.incompatible) > 0:
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

// compatOutcomes holds the outcome for each fingerprint, other than the spec's
// own, of a runner that an agent routes the workload to.
type compatOutcomes map[string]compat

// schemaCompat gives the outcome for a runner fingerprint. It reads the
// outcomes computed beforehand and does no work of its own.
type schemaCompat struct {
	workload model.Workload
	outcomes compatOutcomes
}

// check gives the placement reason for a runner that publishes fingerprint:
//   - empty                          schema_unpublished
//   - the spec's own fingerprint     compatible
//   - no outcome (not in the catalog, or not seen before the plan)  schema_pending
//   - otherwise                      the outcome: compatible, or schema_incompatible
func (c *schemaCompat) check(fingerprint string) compat {
	if fingerprint == "" {
		return compat{reason: "schema_unpublished"}
	}
	if fingerprint == c.workload.SchemaFingerprint {
		return compat{reason: "compatible"}
	}
	if got, ok := c.outcomes[fingerprint]; ok {
		return got
	}
	return compat{reason: "schema_pending"}
}

// outcomesFor validates the spec's body against the schema of the runner each
// agent routes the workload to (see routedRunner), once per fingerprint: the
// schemas of the other runners are not even loaded. A schema podium refuses, or
// that cannot be loaded, makes that fingerprint schema_incompatible; it never
// fails the plan. A nil source knows no schema, so every fingerprint is pending.
func outcomesFor(ctx context.Context, schemas SchemaSource, logger zerolog.Logger, ts *model.Spec, agents []*model.Agent) compatOutcomes {
	out := compatOutcomes{}
	w := ts.Workload()
	want, err := w.GVK()
	if err != nil || schemas == nil {
		return out
	}
	selector := ts.RunnerLabels()
	for _, a := range agents {
		if a == nil {
			continue
		}
		routed, fp := routedRunner(a.Capabilities(), want, selector)
		if routed < 0 || fp == "" || fp == w.SchemaFingerprint {
			continue
		}
		if _, done := out[fp]; !done {
			out[fp] = outcomeFor(ctx, schemas, logger, fp, w.Body)
		}
	}
	return out
}

// routedRunner returns the index in runners of the runner an agent routes a
// task of type want to, as the SDK router does: the first, in declared order,
// that declares want's exact group, version and kind and whose labels match
// every one of selector, whatever its schema. It returns the fingerprint that
// runner declares for the type too, and -1 when no runner matches both.
func routedRunner(runners []model.RunnerCapability, want model.GroupVersionKind, selector map[string]string) (int, string) {
	for i, r := range runners {
		if fp, ok := declaredFingerprint(r, want); ok && labelsMatch(r.Labels, selector) {
			return i, fp
		}
	}
	return -1, ""
}

// declaredFingerprint reports whether runner declares want's exact group,
// version and kind, and the fingerprint it declares for the type.
func declaredFingerprint(runner model.RunnerCapability, want model.GroupVersionKind) (string, bool) {
	for _, gvk := range runner.Workloads {
		if gvk.Group == want.Group && gvk.Version == want.Version && gvk.Kind == want.Kind {
			return gvk.SchemaFingerprint, true
		}
	}
	return "", false
}

// labelsMatch reports whether labels hold every key of selector with its value.
func labelsMatch(labels, selector map[string]string) bool {
	for k, v := range selector {
		if actual, ok := labels[k]; !ok || actual != v {
			return false
		}
	}
	return true
}

// outcomeFor checks the body against the schema of one fingerprint. A schema
// podium refuses shows its reason; an error behind a schema that cannot be
// loaded goes to the log with the fingerprint, and the detail stays generic.
func outcomeFor(ctx context.Context, schemas SchemaSource, logger zerolog.Logger, fingerprint string, body map[string]any) compat {
	schema, known, err := schemas.Compiled(ctx, fingerprint)
	var refused *workload.RefusedError
	switch {
	case errors.As(err, &refused):
		return compat{reason: "schema_incompatible", detail: "podium refused this schema: " + refused.Reason}
	case err != nil:
		logger.Error().Err(err).Str("fingerprint", fingerprint).Msg("placement: the workload schema could not be loaded")
		return compat{reason: "schema_incompatible", detail: "the schema could not be loaded"}
	case !known:
		return compat{reason: "schema_pending"}
	}
	if body == nil {
		body = map[string]any{}
	}
	return outcomeOfValidation(schema.Validate(body))
}

// outcomeOfValidation turns the result of validating a body into an outcome.
func outcomeOfValidation(err error) compat {
	var verr *workloadschema.ValidationError
	switch {
	case err == nil:
		return compat{reason: "compatible"}
	case errors.Is(err, workloadschema.ErrValidationTooExpensive):
		return compat{reason: "schema_incompatible", detail: "the body is too costly to check against this schema"}
	case errors.As(err, &verr):
		return compat{reason: "schema_incompatible", detail: problemsText(verr)}
	default:
		return compat{reason: "schema_incompatible", detail: err.Error()}
	}
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

// planTarget evaluates agent a as a target of ts. Of its runners only the one
// the agent routes the workload to is evaluated (see [model.PlacementTarget]):
// the target's eligibility and reason are that runner's.
func planTarget(a *model.Agent, ts *model.Spec, now time.Time, cmp *schemaCompat) model.PlacementTarget {
	t := model.PlacementTarget{Status: "unknown", HeartbeatFreshness: "unknown", RunnerNames: []string{}, Runners: []model.PlacementRunner{}}
	if a == nil {
		t.EligibilityReason = "missing_agent"
		return t
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
		// An agent that has reported no runners cannot run the workload: nothing
		// says which schema it would check the body against.
		t.EligibilityReason = "inventory_unknown"
		return t
	}
	// The workload type is matched exactly. A spec without a workload has no
	// type, and matches no runner.
	want, wantErr := cmp.workload.GVK()
	selector := ts.RunnerLabels()
	routed := -1
	if wantErr == nil {
		routed, _ = routedRunner(capabilities, want, selector)
	}
	anyWorkload := false
	for i, capability := range capabilities {
		r := model.PlacementRunner{Name: capability.Name, Labels: capability.Labels, LabelsMatch: labelsMatch(capability.Labels, selector)}
		var fingerprint string
		if wantErr == nil {
			fingerprint, r.WorkloadMatch = declaredFingerprint(capability, want)
		}
		anyWorkload = anyWorkload || r.WorkloadMatch
		switch {
		case !r.WorkloadMatch:
			r.Reason = "workload_unsupported"
		case !r.LabelsMatch:
			r.Reason = "runner_labels_mismatch"
		case i == routed:
			out := cmp.check(fingerprint)
			r.Reason, r.Detail, r.Eligible = out.reason, out.detail, out.reason == "compatible"
		}
		// A runner after the routed one whose type and labels match is not
		// evaluated: the agent never routes the workload to it.
		t.Runners = append(t.Runners, r)
	}
	switch {
	case routed >= 0 && t.Runners[routed].Eligible:
		t.Eligible = true
		t.SelectedRunner = t.Runners[routed].Name
		t.RunnerNames = append(t.RunnerNames, t.SelectedRunner)
		t.EligibilityReason = "compatible_runner"
	case routed >= 0:
		t.EligibilityReason = t.Runners[routed].Reason
	case anyWorkload:
		t.EligibilityReason = "runner_labels_mismatch"
	default:
		t.EligibilityReason = "workload_unsupported"
	}
	return t
}

// blocked is the targets that stop a deploy.
type blocked struct {
	// missing are agents absent from discovery.
	missing []string
	// unsupported are agents with no runner for the workload type or its labels.
	unsupported []string
	// incompatible are agents whose runners cannot take the body: no inventory,
	// or no schema the body fits. reasons gives each one's placement reason.
	incompatible []string
	reasons      map[string]string
}

// blockedTargets sorts the targets of a plan that are not eligible.
func blockedTargets(plan *model.PlacementPlan) blocked {
	b := blocked{reasons: map[string]string{}}
	for _, target := range plan.Targets {
		switch {
		case !target.Exists:
			b.missing = append(b.missing, target.AgentID)
		case target.Eligible:
		case isIncompatibleReason(target.EligibilityReason):
			b.incompatible = append(b.incompatible, target.AgentID)
			b.reasons[target.AgentID] = target.EligibilityReason
		default:
			b.unsupported = append(b.unsupported, target.AgentID)
		}
	}
	return b
}

func isIncompatibleReason(reason string) bool {
	switch reason {
	case "inventory_unknown", "schema_incompatible", "schema_pending", "schema_unpublished":
		return true
	}
	return false
}
