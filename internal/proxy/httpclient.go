package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// httpClient is the subset of *http.Client used by the proxy helpers.
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// bearerClient decorates an httpClient, adding "Authorization: Bearer <token>"
// to every request. Used for outbound calls to agents that require a token.
// Only constructed with a non-empty token (see Pool.getV1).
type bearerClient struct {
	inner httpClient
	token string
}

func (b *bearerClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.inner.Do(req)
}

// maxErrorBodyBytes caps how much of a non-2xx response body is read back for
// diagnostic purposes. The SDK emits compact JSON error bodies (~ tens of bytes);
// 4 KiB leaves enough headroom for a stack-style message without letting a
// misbehaving agent balloon our log lines.
const maxErrorBodyBytes = 4 * 1024

// sdkErrorBody is the HTTP error envelope emitted by solti-api:
// {"error":"<label>","message":"<detail>"}. We tolerate missing/extra fields
// so a broken or older agent is still diagnosable.
type sdkErrorBody struct {
	Error   string `json:"error"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// unexpectedStatusError carries the agent's HTTP status code so the transport
// layer (errkind) can recover the agent's semantics. Unwrap exposes
// ErrUnexpectedStatus, so existing errors.Is checks keep working.
type unexpectedStatusError struct {
	code int
	msg  string
}

func (e *unexpectedStatusError) Error() string { return e.msg }

func (e *unexpectedStatusError) Unwrap() error { return ErrUnexpectedStatus }

func (e *unexpectedStatusError) HTTPStatus() int { return e.code }

// formatUnexpectedStatus reads a bounded preview of the response body and
// returns an [unexpectedStatusError] that surfaces the SDK's structured
// {error,message} payload when present (or the raw snippet otherwise) and
// carries the agent's HTTP status code. Bytes consumed here are lost for
// further processing, so callers must only invoke this on the non-success branch.
func formatUnexpectedStatus(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	base := ErrUnexpectedStatus.Error()

	msg := fmt.Sprintf("%s: %d", base, resp.StatusCode)
	if len(body) > 0 {
		var e sdkErrorBody
		if err := json.Unmarshal(body, &e); err == nil && (e.Error != "" || e.Reason != "" || e.Message != "") {
			if e.Error == "" {
				e.Error = e.Reason
			}
			switch {
			case e.Error != "" && e.Message != "":
				msg = fmt.Sprintf("%s: %d %s: %s", base, resp.StatusCode, e.Error, e.Message)
			case e.Error != "":
				msg = fmt.Sprintf("%s: %d %s", base, resp.StatusCode, e.Error)
			default:
				msg = fmt.Sprintf("%s: %d %s", base, resp.StatusCode, e.Message)
			}
		} else {
			msg = fmt.Sprintf("%s: %d: %s", base, resp.StatusCode, string(body))
		}
	}
	return &unexpectedStatusError{code: resp.StatusCode, msg: msg}
}
