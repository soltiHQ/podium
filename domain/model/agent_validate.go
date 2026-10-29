package model

import (
	"fmt"
	"net"
	"net/url"
	"path"
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

// agentIDPattern is the SDK AgentId charset.
var agentIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateAgentID applies the SDK AgentId rule: 1-128 bytes of [A-Za-z0-9._-],
// other than "." and "..".
func ValidateAgentID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: %w", domain.ErrInvalidArgument, domain.ErrEmptyID)
	case len(id) > agentIDMaxBytes:
		return fmt.Errorf("%w: agent id exceeds %d bytes", domain.ErrInvalidArgument, agentIDMaxBytes)
	case id == "." || id == "..":
		return fmt.Errorf("%w: agent id cannot be \".\" or \"..\"", domain.ErrInvalidArgument)
	case !agentIDPattern.MatchString(id):
		return fmt.Errorf("%w: agent id may contain only [A-Za-z0-9._-]", domain.ErrInvalidArgument)
	}
	return nil
}

// ValidateAgentEndpoint accepts the address forms the proxy can call:
//   - HTTP: an absolute http or https URL with a host, an optional port
//     1-65535 and an optional reverse-proxy base path; no userinfo, query or
//     fragment. A base path is absolute, has no empty, "." or ".." segments
//     and at most one trailing "/". It must not need percent-encoding:
//     encoded bytes such as %2F or %2E are rejected rather than decoded,
//     because a proxy in front of the agent may decode them differently.
//   - gRPC: "host:port" with a port 1-65535 and an optional trailing "/".
//     The "http://" and "https://" prefixes that the proxy strips before
//     dialing are accepted too; they do not select TLS.
func ValidateAgentEndpoint(endpoint string, t enum.EndpointType) error {
	switch t {
	case enum.EndpointHTTP:
		u, err := parseAgentURL(endpoint)
		if err != nil {
			return err
		}
		if !validBasePath(u) {
			return invalidEndpoint(`base path must be absolute, without empty, "." or ".." segments or percent-encoding`)
		}
		return nil
	case enum.EndpointGRPC:
		target := endpoint
		if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
			target = "http://" + target
		}
		u, err := parseAgentURL(target)
		if err != nil {
			return err
		}
		if u.Path != "" && u.Path != "/" {
			return invalidEndpoint("must not contain a path")
		}
		if u.Port() == "" {
			return invalidEndpoint("requires a port")
		}
		return nil
	default:
		return fmt.Errorf("%w: %w", domain.ErrInvalidArgument, domain.ErrUnknownEndpointType)
	}
}

// validBasePath reports whether u's path is empty, "/", or a clean absolute
// path with at most one trailing "/" and nothing that needs percent-encoding.
func validBasePath(u *url.URL) bool {
	if u.Path == "" || u.Path == "/" {
		return true
	}
	if strings.Contains(u.EscapedPath(), "%") {
		return false
	}
	trimmed := strings.TrimSuffix(u.Path, "/")
	return trimmed != "/" && path.Clean(trimmed) == trimmed
}

// parseAgentURL parses endpoint under the scheme, credential and host:port
// rules shared by both endpoint types. Path rules are per endpoint type.
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
	}
	host := u.Host
	// A port follows the last ':' only when that ':' comes after any ']': the
	// colons of a bracketed IPv6 literal such as "[::1]" are not a port
	// separator. A bare IPv6 host like "::1:8085" has a ':' last too, but
	// SplitHostPort rejects it, so an unbracketed IPv6 literal never passes.
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
