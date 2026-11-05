package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/enum"
)

// The rule is the SDK AgentId rule (solti-model validate_identity).
func TestValidateAgentIDRejectsUnsafeCharacters(t *testing.T) {
	for _, id := range []string{`x'"`, "a/b", `a\b`, "", strings.Repeat("a", 129), "agent with space", ".", "..", "agent\n", "é"} {
		if err := ValidateAgentID(id); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ValidateAgentID(%q) = %v, want ErrInvalidArgument", id, err)
		}
	}
	for _, id := range []string{"local-http-agent", "550e8400-e29b-41d4-a716-446655440000", "node-1.example.com", "worker_pod-7b9f4", strings.Repeat("a", 128), "_agent", "-agent", ".agent", "..x"} {
		if err := ValidateAgentID(id); err != nil {
			t.Errorf("ValidateAgentID(%q) = %v, want nil", id, err)
		}
	}
}

// Addresses the SDK advertises, plus the gRPC forms normalizeGrpcTarget accepts.
func TestValidateAgentEndpointAcceptsSDKForms(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		kind     enum.EndpointType
	}{
		{"http://127.0.0.1:8085", enum.EndpointHTTP},
		{"http://10.0.0.5:8085", enum.EndpointHTTP},
		{"https://agent.example.com:8443", enum.EndpointHTTP},
		{"http://[::1]:8085", enum.EndpointHTTP},
		{"http://agent.example.com", enum.EndpointHTTP},
		{"http://127.0.0.1:8085/", enum.EndpointHTTP},
		{"http://10.0.0.5:1", enum.EndpointHTTP},
		{"http://10.0.0.5:65535", enum.EndpointHTTP},
		{"127.0.0.1:50051", enum.EndpointGRPC},
		{"10.0.0.5:1", enum.EndpointGRPC},
		{"[::1]:65535", enum.EndpointGRPC},
		{"localhost:50052", enum.EndpointGRPC},
		{"[::1]:50051", enum.EndpointGRPC},
		{"localhost:50052/", enum.EndpointGRPC},
		{"http://localhost:50052", enum.EndpointGRPC},
		{"https://agent.example.com:443/", enum.EndpointGRPC},
	} {
		if err := ValidateAgentEndpoint(tc.endpoint, tc.kind); err != nil {
			t.Errorf("ValidateAgentEndpoint(%q, %s) = %v, want nil", tc.endpoint, tc.kind, err)
		}
	}
}

// Reverse-proxy base paths the SDK documents for advertised HTTP agent URLs.
func TestValidateAgentEndpointAcceptsBasePath(t *testing.T) {
	for _, endpoint := range []string{
		"http://h:1/admin",
		"https://gw.example.com/agents/a1",
		"https://gw.example.com/agents/a1/",
		"http://10.0.0.5:8085/solti/node-1.example.com_x",
	} {
		if err := ValidateAgentEndpoint(endpoint, enum.EndpointHTTP); err != nil {
			t.Errorf("ValidateAgentEndpoint(%q, http) = %v, want nil", endpoint, err)
		}
	}
}

func TestValidateAgentEndpointRejectsUnsafeForms(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		kind     enum.EndpointType
	}{
		{"http://[", enum.EndpointHTTP},
		{"http://u:p@h:1", enum.EndpointHTTP},
		{"http://h:1/admin#", enum.EndpointHTTP},
		{"http://h:1?x=1", enum.EndpointHTTP},
		{"ftp://h:1", enum.EndpointHTTP},
		{"http://:80", enum.EndpointHTTP},
		{"", enum.EndpointHTTP},
		{"h:1", enum.EndpointHTTP},
		{"http://h:1/agents/../admin", enum.EndpointHTTP},
		{"http://h:1/agents/./a1", enum.EndpointHTTP},
		{"http://h:1/agents/..", enum.EndpointHTTP},
		{"http://h:1//admin", enum.EndpointHTTP},
		{"http://h:1/agents//a1", enum.EndpointHTTP},
		{"http://h:1/agents/a1//", enum.EndpointHTTP},
		{"http://h:1//", enum.EndpointHTTP},
		{"http://h:1/%2e%2e/admin", enum.EndpointHTTP},
		{"http://h:1/agents%2Fa1", enum.EndpointHTTP},
		{"http://h:1/agents%20a1", enum.EndpointHTTP},
		{"http://h:1/agents a1", enum.EndpointHTTP},
		{"http://h:1?", enum.EndpointHTTP},
		{"http://h:1#", enum.EndpointHTTP},
		{"http://h:0", enum.EndpointHTTP},
		{"http://h:65536", enum.EndpointHTTP},
		{"http://h:", enum.EndpointHTTP},
		{"http://::1:8085", enum.EndpointHTTP},
		{"h", enum.EndpointGRPC},
		{"h:0", enum.EndpointGRPC},
		{"http://h", enum.EndpointGRPC},
		{"u@h:1", enum.EndpointGRPC},
		{"h:1/admin", enum.EndpointGRPC},
		{"h:1?x=1", enum.EndpointGRPC},
		{"unix:///run/agent.sock", enum.EndpointGRPC},
		{"dns:///h:1", enum.EndpointGRPC},
		{"::1:50051", enum.EndpointGRPC},
		{"http://h:1", enum.EndpointType("smtp")},
	} {
		if err := ValidateAgentEndpoint(tc.endpoint, tc.kind); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ValidateAgentEndpoint(%q, %s) = %v, want ErrInvalidArgument", tc.endpoint, tc.kind, err)
		}
	}
}

// The capability fields of a sync are bounded one by one: the runners, and per
// runner its name, labels and workloads. Each row is a value at its limit, which
// passes, or one over it, which is refused with the field named. The runner and
// the workload that break a limit come second, to show the index in the name.
// No byte limit is below the SDK's own (a label key and an api_version of 317
// bytes), because the SDK treats a refusal as permanent; the counts are caps that
// the SDK does not have.
func TestNewAgentFromEnforcesCapabilityLimits(t *testing.T) {
	text := func(n int) string { return strings.Repeat("x", n) }
	// labels builds entries with distinct zero-padded keys of exactly keyBytes.
	labels := func(entries, keyBytes, valueBytes int) map[string]string {
		m := make(map[string]string, entries)
		for i := range entries {
			m[fmt.Sprintf("%0*d", keyBytes, i)] = text(valueBytes)
		}
		return m
	}
	workloads := func(n int) []GroupVersionKind {
		out := make([]GroupVersionKind, n)
		for i := range out {
			out[i] = GroupVersionKind{Group: "solti.io", Version: "v1", Kind: "Subprocess"}
		}
		return out
	}
	// second sets the runner that follows a plain one.
	second := func(edit func(*RunnerCapability)) func(*AgentParams) {
		return func(p *AgentParams) {
			r := RunnerCapability{Name: "second", Labels: map[string]string{}, Workloads: workloads(2)}
			edit(&r)
			p.Capabilities = []RunnerCapability{{Name: "first", Labels: map[string]string{}}, r}
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*AgentParams)
		rule string // what the refusal says; empty for a value that passes
	}{
		{"runners at limit", func(p *AgentParams) { p.Capabilities = make([]RunnerCapability, 64) }, ""},
		{"too many runners", func(p *AgentParams) { p.Capabilities = make([]RunnerCapability, 65) }, "agent capabilities exceed 64 runners"},
		{"runner name at limit", second(func(r *RunnerCapability) { r.Name = text(128) }), ""},
		{"runner name over limit", second(func(r *RunnerCapability) { r.Name = text(129) }), "agent capabilities.runners[1].name exceeds 128 bytes"},

		{"labels at limits", second(func(r *RunnerCapability) { r.Labels = labels(256, 317, 1024) }), ""},
		{"too many labels", second(func(r *RunnerCapability) { r.Labels = labels(257, 8, 1) }), "agent capabilities.runners[1].labels exceed 256 entries"},
		{"label key over limit", second(func(r *RunnerCapability) { r.Labels = labels(1, 318, 1) }), "agent capabilities.runners[1].labels has a key over 317 bytes"},
		{"label value over limit", second(func(r *RunnerCapability) { r.Labels = labels(1, 8, 1025) }), "agent capabilities.runners[1].labels has a value over 1024 bytes"},

		{"workloads at limit", second(func(r *RunnerCapability) { r.Workloads = workloads(256) }), ""},
		{"too many workloads", second(func(r *RunnerCapability) { r.Workloads = workloads(257) }), "agent capabilities.runners[1].workloads exceed 256 entries"},
		// api_version is the group, a slash and the version: 253 + 1 + 63 bytes.
		{"api version with a group at limit", second(func(r *RunnerCapability) {
			r.Workloads[1] = GroupVersionKind{Group: text(253), Version: text(63), Kind: "Subprocess"}
		}), ""},
		{"api version with a group over limit", second(func(r *RunnerCapability) {
			r.Workloads[1] = GroupVersionKind{Group: text(253), Version: text(64), Kind: "Subprocess"}
		}), "agent capabilities.runners[1].workloads[1].api_version exceeds 317 bytes"},
		{"api version without a group at limit", second(func(r *RunnerCapability) {
			r.Workloads[1] = GroupVersionKind{Version: text(317), Kind: "Subprocess"}
		}), ""},
		{"api version without a group over limit", second(func(r *RunnerCapability) {
			r.Workloads[1] = GroupVersionKind{Version: text(318), Kind: "Subprocess"}
		}), "agent capabilities.runners[1].workloads[1].api_version exceeds 317 bytes"},
		{"kind at limit", second(func(r *RunnerCapability) { r.Workloads[1].Kind = text(128) }), ""},
		{"kind over limit", second(func(r *RunnerCapability) { r.Workloads[1].Kind = text(129) }), "agent capabilities.runners[1].workloads[1].kind exceeds 128 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := AgentParams{ID: "agent-1", Name: "Agent", Endpoint: "http://10.0.0.5:8085", EndpointType: 2, APIVersion: 1}
			tc.edit(&p)
			_, err := NewAgentFrom(p)
			switch {
			case tc.rule == "" && err != nil:
				t.Fatalf("NewAgentFrom rejected a value at its limit: %v", err)
			case tc.rule != "" && !errors.Is(err, domain.ErrInvalidArgument):
				t.Fatalf("NewAgentFrom = %v, want ErrInvalidArgument", err)
			case tc.rule != "" && err.Error() != domain.ErrInvalidArgument.Error()+": "+tc.rule:
				t.Fatalf("NewAgentFrom = %q, want it to say %q", err, tc.rule)
			}
		})
	}
}

// os, arch and platform are cut to 256 bytes, never refused: os is the
// PRETTY_NAME of the host, which the SDK does not bound, and the SDK treats a
// refusal as permanent. The cut falls on a rune boundary and adds nothing.
func TestNewAgentFromCutsPlatformFields(t *testing.T) {
	text := func(n int) string { return strings.Repeat("o", n) }
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"short", "Debian GNU/Linux 12 (bookworm)", "Debian GNU/Linux 12 (bookworm)"},
		{"at limit", text(256), text(256)},
		{"over limit", text(300), text(256)},
		{"a rune that ends at the limit", text(254) + "é" + text(40), text(254) + "é"},
		{"a 2-byte rune over the limit", text(255) + "é" + text(40), text(255)},
		{"a 3-byte rune over the limit", text(254) + "€" + text(40), text(254)},
		{"a 4-byte rune over the limit", text(253) + "😀" + text(40), text(253)},
	} {
		for field, set := range map[string]struct {
			edit func(*AgentParams, string)
			read func(*Agent) string
		}{
			"os":       {func(p *AgentParams, v string) { p.OS = v }, (*Agent).OS},
			"arch":     {func(p *AgentParams, v string) { p.Arch = v }, (*Agent).Arch},
			"platform": {func(p *AgentParams, v string) { p.Platform = v }, (*Agent).Platform},
		} {
			t.Run(field+", "+tc.name, func(t *testing.T) {
				p := AgentParams{ID: "agent-1", Name: "Agent", Endpoint: "http://10.0.0.5:8085", EndpointType: 2, APIVersion: 1}
				set.edit(&p, tc.in)
				a, err := NewAgentFrom(p)
				if err != nil {
					t.Fatalf("NewAgentFrom refused a long %s: %v", field, err)
				}
				if got := set.read(a); got != tc.want || !utf8.ValidString(got) {
					t.Fatalf("%s stored as %d bytes %q, want %d bytes %q", field, len(got), got, len(tc.want), tc.want)
				}
			})
		}
	}
}

func TestCutUTF8(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"empty", "", 4, ""},
		{"shorter", "abc", 4, "abc"},
		{"exactly the limit", "abcd", 4, "abcd"},
		{"ascii over", "abcdef", 4, "abcd"},
		{"cut falls between runes", "abéz", 4, "abé"},
		{"cut splits a 2-byte rune", "abcé", 4, "abc"},
		{"cut splits a 3-byte rune at its second byte", "ab€", 3, "ab"},
		{"cut splits a 3-byte rune at its third byte", "ab€", 4, "ab"},
		{"cut splits a 4-byte rune at its second byte", "a😀", 2, "a"},
		{"cut splits a 4-byte rune at its fourth byte", "a😀", 4, "a"},
		{"limit zero", "abc", 0, ""},
		// Not UTF-8: the step back is bounded, so it cannot cut to nothing.
		{"continuation bytes only", "\x80\x80\x80\x80\x80\x80", 5, "\x80\x80"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cutUTF8(tc.in, tc.limit); got != tc.want {
				t.Fatalf("cutUTF8(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
			}
		})
	}
}

func TestNewAgentFromEnforcesFieldLimits(t *testing.T) {
	// metadata builds entries with distinct zero-padded keys of exactly keyBytes.
	metadata := func(entries, keyBytes, valueBytes int) map[string]string {
		m := make(map[string]string, entries)
		for i := range entries {
			m[fmt.Sprintf("%0*d", keyBytes, i)] = strings.Repeat("v", valueBytes)
		}
		return m
	}
	for _, tc := range []struct {
		name  string
		edit  func(*AgentParams)
		valid bool
	}{
		{"heartbeat unset", func(p *AgentParams) { p.HeartbeatIntervalS = 0 }, true},
		{"heartbeat one second", func(p *AgentParams) { p.HeartbeatIntervalS = 1 }, true},
		{"heartbeat one hour", func(p *AgentParams) { p.HeartbeatIntervalS = 3600 }, true},
		{"negative heartbeat", func(p *AgentParams) { p.HeartbeatIntervalS = -1 }, false},
		{"heartbeat over one hour", func(p *AgentParams) { p.HeartbeatIntervalS = 3601 }, false},
		{"name at limit", func(p *AgentParams) { p.Name = strings.Repeat("n", 256) }, true},
		{"name over limit", func(p *AgentParams) { p.Name = strings.Repeat("n", 257) }, false},
		{"metadata at limits", func(p *AgentParams) { p.Metadata = metadata(64, 128, 1024) }, true},
		{"too many metadata entries", func(p *AgentParams) { p.Metadata = metadata(65, 8, 1) }, false},
		{"metadata key over limit", func(p *AgentParams) { p.Metadata = metadata(1, 129, 1) }, false},
		{"metadata value over limit", func(p *AgentParams) { p.Metadata = metadata(1, 8, 1025) }, false},
		{"empty id", func(p *AgentParams) { p.ID = "" }, false},
		{"unsafe id", func(p *AgentParams) { p.ID = "a/b" }, false},
		{"unparsable endpoint", func(p *AgentParams) { p.Endpoint = "http://[" }, false},
		{"missing endpoint", func(p *AgentParams) { p.Endpoint = "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := AgentParams{ID: "agent-1", Name: "Agent", Endpoint: "http://10.0.0.5:8085", EndpointType: 2, APIVersion: 1}
			tc.edit(&p)
			_, err := NewAgentFrom(p)
			if tc.valid && err != nil {
				t.Fatalf("NewAgentFrom rejected valid params: %v", err)
			}
			if !tc.valid && !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("NewAgentFrom = %v, want ErrInvalidArgument", err)
			}
		})
	}
}
