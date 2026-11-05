package model

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

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

// Limits for the platform and capability fields of a discovery payload: os,
// arch and platform, and the runners with their names, labels and workloads.
//
// The SDK treats a refusal, HTTP 400 or gRPC InvalidArgument, as permanent, so
// none of these is below what the SDK itself lets an agent send. A label key
// and an api_version are qualified names there: a DNS prefix of 253 bytes, a
// slash and 63 bytes, which is 317; its names, label values and kinds are
// shorter than the limits here; and it sets no count limit. os, arch and
// platform are cut, never refused: os is the PRETTY_NAME of the host's
// /etc/os-release, which the SDK does not bound.
const (
	agentPlatformMaxBytes           = 256 // os, arch and platform are cut to this, each
	agentRunnersMax                 = 64
	agentRunnerNameMaxBytes         = 128
	agentRunnerLabelsMax            = 256
	agentRunnerLabelKeyMaxBytes     = 317
	agentRunnerLabelValueMaxBytes   = 1024
	agentRunnerWorkloadsMax         = 256
	agentWorkloadAPIVersionMaxBytes = 317
	agentWorkloadKindMaxBytes       = 128
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
	return validateCapabilities(p.Capabilities)
}

// cutUTF8 returns s cut to at most limit bytes, on a rune boundary: when the
// cut would split a rune, it falls before that rune. Nothing is added to the
// text. It steps back at most three bytes, so that text that is not UTF-8 cannot
// make it cut to nothing.
func cutUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for back := 0; back < utf8.UTFMax-1 && limit > 0 && !utf8.RuneStart(s[limit]); back++ {
		limit--
	}
	return s[:limit]
}

// validateCapabilities bounds the runner inventory of a discovery payload: the
// runners, and per runner its name, labels and workloads. A refusal names the
// field, as the SDK's SyncRequest does, with the index of the runner or the
// workload.
func validateCapabilities(runners []RunnerCapability) error {
	if len(runners) > agentRunnersMax {
		return fmt.Errorf("%w: agent capabilities exceed %d runners", domain.ErrInvalidArgument, agentRunnersMax)
	}
	for i, r := range runners {
		if err := validateRunner(i, r); err != nil {
			return err
		}
	}
	return nil
}

func validateRunner(i int, r RunnerCapability) error {
	switch {
	case len(r.Name) > agentRunnerNameMaxBytes:
		return invalidRunner(i, ".name exceeds %d bytes", agentRunnerNameMaxBytes)
	case len(r.Labels) > agentRunnerLabelsMax:
		return invalidRunner(i, ".labels exceed %d entries", agentRunnerLabelsMax)
	case len(r.Workloads) > agentRunnerWorkloadsMax:
		return invalidRunner(i, ".workloads exceed %d entries", agentRunnerWorkloadsMax)
	}
	for k, v := range r.Labels {
		switch {
		case len(k) > agentRunnerLabelKeyMaxBytes:
			return invalidRunner(i, ".labels has a key over %d bytes", agentRunnerLabelKeyMaxBytes)
		case len(v) > agentRunnerLabelValueMaxBytes:
			return invalidRunner(i, ".labels has a value over %d bytes", agentRunnerLabelValueMaxBytes)
		}
	}
	for j, w := range r.Workloads {
		switch {
		case apiVersionBytes(w) > agentWorkloadAPIVersionMaxBytes:
			return invalidRunner(i, ".workloads[%d].api_version exceeds %d bytes", j, agentWorkloadAPIVersionMaxBytes)
		case len(w.Kind) > agentWorkloadKindMaxBytes:
			return invalidRunner(i, ".workloads[%d].kind exceeds %d bytes", j, agentWorkloadKindMaxBytes)
		}
	}
	return nil
}

// apiVersionBytes is the length of the api_version of a workload: the version,
// preceded by the group and a slash when there is a group.
func apiVersionBytes(w GroupVersionKind) int {
	if w.Group == "" {
		return len(w.Version)
	}
	return len(w.Group) + 1 + len(w.Version)
}

func invalidRunner(i int, format string, args ...any) error {
	return fmt.Errorf("%w: agent capabilities.runners[%d]%s", domain.ErrInvalidArgument, i, fmt.Sprintf(format, args...))
}
