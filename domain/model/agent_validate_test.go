package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/enum"
)

func TestValidateAgentIDRejectsUnsafeCharacters(t *testing.T) {
	for _, id := range []string{`x'"`, "a/b", "", strings.Repeat("a", 129), "agent with space", "-agent", "..", "agent\n"} {
		if err := ValidateAgentID(id); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ValidateAgentID(%q) = %v, want ErrInvalidArgument", id, err)
		}
	}
	for _, id := range []string{"local-http-agent", "550e8400-e29b-41d4-a716-446655440000", "node-1.example.com", "worker_pod-7b9f4", strings.Repeat("a", 128)} {
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
		{"127.0.0.1:50051", enum.EndpointGRPC},
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
		{"http://h:1/admin", enum.EndpointHTTP},
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
