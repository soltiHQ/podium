package httpctx

import (
	"context"
	"testing"
)

func TestLeader(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"no value", context.Background(), ""},
		{"a nil function", WithLeader(context.Background(), nil), ""},
		{"no leader known", WithLeader(context.Background(), func() string { return "" }), ""},
		{"a leader", WithLeader(context.Background(), func() string { return "10.0.0.2:8080" }), "10.0.0.2:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Leader(tt.ctx); got != tt.want {
				t.Fatalf("Leader() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The leader is asked when the answer is written, not when the request starts:
// leadership can move while the request runs.
func TestLeaderIsAskedEachTime(t *testing.T) {
	leader := "10.0.0.2:8080"
	ctx := WithLeader(context.Background(), func() string { return leader })
	if got := Leader(ctx); got != "10.0.0.2:8080" {
		t.Fatalf("Leader() = %q", got)
	}
	leader = "10.0.0.3:8080"
	if got := Leader(ctx); got != "10.0.0.3:8080" {
		t.Fatalf("Leader() after the leader moved = %q", got)
	}
}
