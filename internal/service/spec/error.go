package spec

import (
	"errors"
	"fmt"
	"strings"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// UnknownTargetsError is returned by Deploy when the spec declares one
// or more agent IDs that don't exist in the agent store. Carries the
// offending list so the handler can surface the problem to the user
// precisely (e.g. "unknown target: agent-xyz" instead of a generic 400).
type UnknownTargetsError struct {
	Agents []string
}

func (e *UnknownTargetsError) Error() string {
	return fmt.Sprintf("deploy rejected: unknown target agents: %s", strings.Join(e.Agents, ", "))
}

// ConflictError is returned by Upsert when the client's expected
// version doesn't match what's currently stored — i.e. another writer
// modified the spec in the meantime. Carries both numbers so the UI
// can show "you saw v3, someone saved v5 in the meantime".
//
// This is the classic optimistic-concurrency guard: clients load a spec
// with its version, edit, and submit that version along with the new
// payload. If storage has advanced past it, we reject instead of
// silently overwriting the intervening changes.
type ConflictError struct {
	Expected int
	Actual   int
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf(
		"version conflict: expected %d, stored is %d (spec was modified concurrently)",
		e.Expected, e.Actual,
	)
}

// UnsupportedTargetsError identifies advertised runner inventories that cannot
// execute the requested workload with its runner-label selector.
type UnsupportedTargetsError struct{ Agents []string }

func (e *UnsupportedTargetsError) Error() string {
	return fmt.Sprintf("deploy rejected: no compatible workload runner on agents: %s", strings.Join(e.Agents, ", "))
}
func (e *UnsupportedTargetsError) Unwrap() error { return storage.ErrInvalidArgument }

// ErrPlacementChanged is returned by Deploy when the caller's plan token no
// longer matches the plan recomputed inside the transaction: since the preview
// the spec was saved, the set of targets changed, a target was registered or
// changed eligibility or selected runner, or another deploy created rollouts.
// Deploy writes nothing; the caller previews again.
var ErrPlacementChanged = errors.New("placement changed since preview")
