package inmemory

import (
	"testing"

	"github.com/soltiHQ/control-plane/domain"
	discoverv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
)

// newTestAgentNoLabels creates an agent model for testing without labels.
func newTestAgentNoLabels(t *testing.T, id, platform, osName, arch string) *domain.AgentModel {
	t.Helper()

	raw := &discoverv1.SyncRequest{
		Id:            id,
		Name:          id + "-name",
		Endpoint:      "http://example.com:8080",
		UptimeSeconds: 42,
		Os:            osName,
		Arch:          arch,
		Platform:      platform,
		Metadata:      map[string]string{},
	}
	a, err := domain.NewAgentModel(raw)
	if err != nil {
		t.Fatalf("NewAgentModel(%q) failed: %v", id, err)
	}
	return a
}

// TestFilter_EmptyFilter verifies that a filter with no predicates matches all agents.
func TestFilter_EmptyFilter(t *testing.T) {
	agents := []*domain.AgentModel{
		newTestAgentNoLabels(t, "a1", "kubernetes", "linux", "amd64"),
		newTestAgentNoLabels(t, "a2", "bare-metal", "darwin", "arm64"),
	}

	filter := NewFilter()
	for _, a := range agents {
		if !filter.Matches(a) {
			t.Errorf("empty filter should match agent %q", a.ID())
		}
	}
}

// TestFilter_ByPlatform verifies platform-based filtering with exact matching.
func TestFilter_ByPlatform(t *testing.T) {
	tests := []struct {
		name     string
		agent    *domain.AgentModel
		platform string
		want     bool
	}{
		{
			name:     "exact match",
			agent:    newTestAgentNoLabels(t, "a1", "kubernetes", "linux", "amd64"),
			platform: "kubernetes",
			want:     true,
		},
		{
			name:     "no match",
			agent:    newTestAgentNoLabels(t, "a2", "bare-metal", "linux", "amd64"),
			platform: "kubernetes",
			want:     false,
		},
		{
			name:     "case sensitive",
			agent:    newTestAgentNoLabels(t, "a3", "Kubernetes", "linux", "amd64"),
			platform: "kubernetes",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := NewFilter().ByPlatform(tt.platform)
			got := filter.Matches(tt.agent)
			if got != tt.want {
				t.Errorf("ByPlatform(%q).Matches() = %v, want %v", tt.platform, got, tt.want)
			}
		})
	}
}

// TestFilter_ByLabel verifies label-based filtering with key-value matching.
func TestFilter_ByLabel(t *testing.T) {
	a1 := newTestAgentNoLabels(t, "a1", "kubernetes", "linux", "amd64")
	a1.LabelAdd("env", "production")
	a1.LabelAdd("team", "platform")

	a2 := newTestAgentNoLabels(t, "a2", "kubernetes", "linux", "amd64")
	a2.LabelAdd("env", "staging")

	a3 := newTestAgentNoLabels(t, "a3", "kubernetes", "linux", "amd64")

	tests := []struct {
		name  string
		agent *domain.AgentModel
		key   string
		value string
		want  bool
	}{
		{
			name:  "exact match",
			agent: a1,
			key:   "env",
			value: "production",
			want:  true,
		},
		{
			name:  "wrong value",
			agent: a1,
			key:   "env",
			value: "staging",
			want:  false,
		},
		{
			name:  "missing key",
			agent: a3,
			key:   "env",
			value: "production",
			want:  false,
		},
		{
			name:  "case sensitive value",
			agent: a1,
			key:   "env",
			value: "Production",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := NewFilter().ByLabel(tt.key, tt.value)
			got := filter.Matches(tt.agent)
			if got != tt.want {
				t.Errorf("ByLabel(%q, %q).Matches() = %v, want %v", tt.key, tt.value, got, tt.want)
			}
		})
	}
}

// TestFilter_ByOS verifies operating system filtering with exact matching.
func TestFilter_ByOS(t *testing.T) {
	tests := []struct {
		name  string
		agent *domain.AgentModel
		os    string
		want  bool
	}{
		{
			name:  "linux match",
			agent: newTestAgentNoLabels(t, "a1", "kubernetes", "linux", "amd64"),
			os:    "linux",
			want:  true,
		},
		{
			name:  "darwin no match",
			agent: newTestAgentNoLabels(t, "a2", "kubernetes", "darwin", "amd64"),
			os:    "linux",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := NewFilter().ByOS(tt.os)
			got := filter.Matches(tt.agent)
			if got != tt.want {
				t.Errorf("ByOS(%q).Matches() = %v, want %v", tt.os, got, tt.want)
			}
		})
	}
}

// TestFilter_ByArch verifies architecture filtering with exact matching.
func TestFilter_ByArch(t *testing.T) {
	tests := []struct {
		name  string
		agent *domain.AgentModel
		arch  string
		want  bool
	}{
		{
			name:  "amd64 match",
			agent: newTestAgentNoLabels(t, "a1", "kubernetes", "linux", "amd64"),
			arch:  "amd64",
			want:  true,
		},
		{
			name:  "arm64 no match",
			agent: newTestAgentNoLabels(t, "a2", "kubernetes", "linux", "arm64"),
			arch:  "amd64",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := NewFilter().ByArch(tt.arch)
			got := filter.Matches(tt.agent)
			if got != tt.want {
				t.Errorf("ByArch(%q).Matches() = %v, want %v", tt.arch, got, tt.want)
			}
		})
	}
}

// TestFilter_MultiplePredicates verifies that all predicates are ANDed together.
func TestFilter_MultiplePredicates(t *testing.T) {
	a1 := newTestAgentNoLabels(t, "a1", "kubernetes", "linux", "amd64")
	a1.LabelAdd("env", "production")

	a2 := newTestAgentNoLabels(t, "a2", "kubernetes", "linux", "arm64")
	a2.LabelAdd("env", "production")

	a3 := newTestAgentNoLabels(t, "a3", "kubernetes", "darwin", "amd64")
	a3.LabelAdd("env", "production")

	a4 := newTestAgentNoLabels(t, "a4", "bare-metal", "linux", "amd64")
	a4.LabelAdd("env", "production")

	tests := []struct {
		name  string
		agent *domain.AgentModel
		want  bool
	}{
		{name: "all match", agent: a1, want: true},
		{name: "arch mismatch", agent: a2, want: false},
		{name: "os mismatch", agent: a3, want: false},
		{name: "platform mismatch", agent: a4, want: false},
	}

	filter := NewFilter().
		ByPlatform("kubernetes").
		ByOS("linux").
		ByArch("amd64").
		ByLabel("env", "production")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filter.Matches(tt.agent)
			if got != tt.want {
				t.Errorf("filter.Matches(%q) = %v, want %v", tt.agent.ID(), got, tt.want)
			}
		})
	}
}

// TestFilter_Chaining verifies that filter methods return the same instance for chaining.
func TestFilter_Chaining(t *testing.T) {
	f1 := NewFilter()
	f2 := f1.ByPlatform("kubernetes")
	f3 := f2.ByOS("linux")

	if f1 != f2 || f2 != f3 {
		t.Error("filter methods should return the same instance for method chaining")
	}
}
