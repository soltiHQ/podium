package cluster_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// Only a write that met the wrong leadership is a leadership error.
func TestIsLeadershipError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"not the leader", cluster.ErrNotLeader, true},
		{"leadership lost", cluster.ErrLeadershipLost, true},
		{"wrapped", fmt.Errorf("raft: apply: %w", cluster.ErrLeadershipLost), true},
		{"joined with its cause", errors.Join(cluster.ErrNotLeader, errors.New("node is not the leader")), true},
		{"another error", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cluster.IsLeadershipError(tc.err); got != tc.want {
				t.Fatalf("IsLeadershipError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
