package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

// AgentError is the proxy's anti-corruption wrapper for an error returned by an
// agent. It translates the agent's wire status (gRPC code / HTTP status) into a
// transport-agnostic errkind.Kind at the boundary, so the rest of the control
// plane classifies it through the single errkind contract — without sniffing
// gRPC/HTTP details downstream.
//
// Its text is capped, see CapAgentText: the text of an agent's error is the agent's
// to choose, so a place that formats the error to store, record or log it holds at
// most that much. The chain behind the error is whole, and a place that reads the
// status inside it reads all of it.
type AgentError struct {
	kind errkind.Kind
	err  error
	msg  string
}

func (e *AgentError) Error() string { return e.msg }

func (e *AgentError) Unwrap() error { return e.err }

func (e *AgentError) ErrorKind() errkind.Kind { return e.kind }

// agentError wraps an error from an agent call: it keeps the op sentinel and the
// underlying error in the chain (for errors.Is) and tags the boundary Kind
// derived from the agent's wire status.
func agentError(op, agentErr error) error {
	err := fmt.Errorf("%w: %w", op, agentErr)
	return &AgentError{
		kind: classifyAgentErr(agentErr),
		err:  err,
		msg:  CapAgentText(err.Error()),
	}
}

// MaxAgentTextBytes is the most of the text of an agent's error that the control
// plane keeps as an error, records in an event or logs, before the marker
// CapAgentText ends a cut text with.
const MaxAgentTextBytes = 4 << 10

// agentTextMarker ends a text that CapAgentText cut.
const agentTextMarker = "…"

// CapAgentText returns the text of an agent's error cut to MaxAgentTextBytes on a
// UTF-8 boundary and ended with a marker where it was cut; a text within the limit
// is returned as it is. An agent may answer with as much text as a message or a
// header list holds. AgentError reads as the cut text, the sync runner cuts the
// message it stores in a rollout and records in an event, which holds words of its
// own besides the agent's, and the history service cuts the quoted status it names
// in the error for a run of a status it does not know, which its runner logs. It
// cuts nothing else an agent produces: the archive of history keeps the error an
// agent reports for a run in a record under a bound of its own,
// ExecutionErrorMaxBytes (32 KiB), and refuses a record over it instead of cutting
// it. The cut keeps whole runes of a valid text; it steps back at most three bytes
// to find a boundary, so bytes that are no UTF-8 are cut just below the limit.
func CapAgentText(s string) string {
	if len(s) <= MaxAgentTextBytes {
		return s
	}
	cut := MaxAgentTextBytes
	for i := 0; i < utf8.UTFMax && !utf8.RuneStart(s[cut]); i++ {
		cut--
	}
	return s[:cut] + agentTextMarker
}

// receiveLimitMessage is how grpc-go words its refusal of a message over the
// receive limit: "grpc: received message larger than max (N vs. M)".
const receiveLimitMessage = "received message larger than max"

// responseError is agentError for a gRPC call that reads an answer, which the
// client may refuse for its size: grpc-go's ResourceExhausted for a message over
// the receive limit. It adds ErrResponseTooLarge to that refusal, and keeps the
// status, so that the Kind stays the one agentError derives. A ResourceExhausted
// from the agent, a quota for one, is not tagged. A call that sends a large
// request does not use it: the agent refuses a request over its limit in the same
// words.
func responseError(op, agentErr error) error {
	if st, ok := status.FromError(agentErr); ok && st.Code() == codes.ResourceExhausted && strings.Contains(st.Message(), receiveLimitMessage) {
		agentErr = fmt.Errorf("%w: %w", agentErr, ErrResponseTooLarge)
	}
	return agentError(op, agentErr)
}

// classifyAgentErr derives an errkind.Kind from an agent's transport error: a
// gRPC status code, or an HTTP status carried by an unexpectedStatusError.
// Anything without a recognizable status (CP-side dial/decode failures) is
// Internal.
func classifyAgentErr(err error) errkind.Kind {
	if st, ok := status.FromError(err); ok && st.Code() != codes.OK {
		return fromGRPCCode(st.Code())
	}
	var hs interface{ HTTPStatus() int }
	if errors.As(err, &hs) {
		return fromHTTPStatus(hs.HTTPStatus())
	}
	return errkind.Internal
}

func fromGRPCCode(c codes.Code) errkind.Kind {
	switch c {
	case codes.InvalidArgument:
		return errkind.InvalidArgument
	case codes.Unauthenticated:
		return errkind.Unauthenticated
	case codes.PermissionDenied:
		return errkind.PermissionDenied
	case codes.NotFound:
		return errkind.NotFound
	case codes.AlreadyExists:
		return errkind.AlreadyExists
	case codes.Aborted:
		return errkind.Conflict
	case codes.FailedPrecondition:
		return errkind.FailedPrecondition
	case codes.Unavailable, codes.ResourceExhausted:
		return errkind.Unavailable
	case codes.Canceled:
		return errkind.Canceled
	case codes.DeadlineExceeded:
		return errkind.DeadlineExceeded
	default:
		return errkind.Internal
	}
}

func fromHTTPStatus(code int) errkind.Kind {
	switch code {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return errkind.InvalidArgument
	case http.StatusUnauthorized:
		return errkind.Unauthenticated
	case http.StatusForbidden:
		return errkind.PermissionDenied
	case http.StatusNotFound:
		return errkind.NotFound
	case http.StatusConflict:
		return errkind.Conflict
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return errkind.Unavailable
	default:
		return errkind.Internal
	}
}
