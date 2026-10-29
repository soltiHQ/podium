package model

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/enum"
)

// Limits for agent-supplied discovery fields.
const (
	agentIDMaxBytes            = 128
	agentNameMaxBytes          = 256
	agentMetadataMaxEntries    = 64
	agentMetadataKeyMaxBytes   = 128
	agentMetadataValueMaxBytes = 1024
	agentHeartbeatMaxSeconds   = 3600
)

// agentIDPattern is the SDK AgentId charset with a letter or digit first.
var agentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateAgentID accepts 1-128 bytes matching ^[A-Za-z0-9][A-Za-z0-9._-]*$.
func ValidateAgentID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: %w", domain.ErrInvalidArgument, domain.ErrEmptyID)
	case len(id) > agentIDMaxBytes:
		return fmt.Errorf("%w: agent id exceeds %d bytes", domain.ErrInvalidArgument, agentIDMaxBytes)
	case !agentIDPattern.MatchString(id):
		return fmt.Errorf("%w: agent id must start with a letter or digit and contain only [A-Za-z0-9._-]", domain.ErrInvalidArgument)
	}
	return nil
}

// ValidateAgentEndpoint accepts the address forms the proxy can call:
//   - HTTP: an absolute http or https URL with a host, an optional port
//     1-65535 and an empty or "/" path; no userinfo, query or fragment.
//   - gRPC: "host:port" with a port 1-65535 and an optional trailing "/".
//     The "http://" and "https://" prefixes that the proxy strips before
//     dialing are accepted too; they do not select TLS.
func ValidateAgentEndpoint(endpoint string, t enum.EndpointType) error {
	switch t {
	case enum.EndpointHTTP:
		_, err := parseAgentURL(endpoint)
		return err
	case enum.EndpointGRPC:
		target := endpoint
		if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
			target = "http://" + target
		}
		u, err := parseAgentURL(target)
		if err != nil {
			return err
		}
		if u.Port() == "" {
			return invalidEndpoint("requires a port")
		}
		return nil
	default:
		return fmt.Errorf("%w: %w", domain.ErrInvalidArgument, domain.ErrUnknownEndpointType)
	}
}

// parseAgentURL parses endpoint under the HTTP endpoint rules.
func parseAgentURL(endpoint string) (*url.URL, error) {
	// A bare "?" or "#" parses to an empty query or fragment; check the raw delimiters.
	if strings.ContainsAny(endpoint, "?#") {
		return nil, invalidEndpoint("must not contain a query or fragment")
	}
	u, err := url.Parse(endpoint)
	switch {
	case err != nil:
		return nil, invalidEndpoint("is not a valid URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, invalidEndpoint("must use http or https")
	case u.User != nil:
		return nil, invalidEndpoint("must not contain credentials")
	case u.Path != "" && u.Path != "/":
		return nil, invalidEndpoint("must not contain a path")
	}
	host := u.Host
	if strings.LastIndexByte(host, ':') > strings.LastIndexByte(host, ']') {
		h, port, err := net.SplitHostPort(host)
		if err != nil {
			return nil, invalidEndpoint("has an invalid host or port")
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, invalidEndpoint("port must be 1-65535")
		}
		host = h
	}
	if strings.Trim(host, "[]") == "" {
		return nil, invalidEndpoint("must contain a host")
	}
	return u, nil
}

func invalidEndpoint(reason string) error {
	return fmt.Errorf("%w: agent endpoint %s", domain.ErrInvalidArgument, reason)
}

// validateAgentParams bounds the discovery-supplied fields of an agent.
func validateAgentParams(p AgentParams, epType enum.EndpointType) error {
	if err := ValidateAgentEndpoint(p.Endpoint, epType); err != nil {
		return err
	}
	if len(p.Name) > agentNameMaxBytes {
		return fmt.Errorf("%w: agent name exceeds %d bytes", domain.ErrInvalidArgument, agentNameMaxBytes)
	}
	if p.HeartbeatIntervalS < 0 || p.HeartbeatIntervalS > agentHeartbeatMaxSeconds {
		return fmt.Errorf("%w: heartbeat interval must be 0 or 1-%d seconds", domain.ErrInvalidArgument, agentHeartbeatMaxSeconds)
	}
	if len(p.Metadata) > agentMetadataMaxEntries {
		return fmt.Errorf("%w: agent metadata exceeds %d entries", domain.ErrInvalidArgument, agentMetadataMaxEntries)
	}
	for k, v := range p.Metadata {
		if len(k) > agentMetadataKeyMaxBytes || len(v) > agentMetadataValueMaxBytes {
			return fmt.Errorf("%w: agent metadata keys are limited to %d bytes and values to %d bytes", domain.ErrInvalidArgument, agentMetadataKeyMaxBytes, agentMetadataValueMaxBytes)
		}
	}
	return nil
}
