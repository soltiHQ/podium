// Package standalone implements a Leadership that is always leader.
// It exists so runners can be wired to cluster.Leadership unchanged in a
// single-node deployment.
package standalone

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

var _ cluster.Leadership = (*Leadership)(nil)

// Leadership reports itself as leader at all times.
type Leadership struct{}

func NewLeadership() *Leadership { return &Leadership{} }

func (l *Leadership) AmLeader() bool        { return true }
func (l *Leadership) CurrentLeader() string { return "" }

func (l *Leadership) WhenLeader(ctx context.Context, fn func(context.Context) error) error {
	if fn == nil {
		return nil
	}
	return fn(ctx)
}
