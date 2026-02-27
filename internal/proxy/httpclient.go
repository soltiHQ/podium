package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// httpClient is the subset of *http.Client used by the proxy helpers.
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// doGet performs a GET request and JSON-decodes the response body.
func doGet[T any](ctx context.Context, client httpClient, url string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCreateRequest, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequest, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}

	var dst T
	if err = json.NewDecoder(resp.Body).Decode(&dst); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	return &dst, nil
}

// doPost performs a POST request with a JSON body [statuses: 200, 201, 204].
func doPost(ctx context.Context, client httpClient, url string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCreateRequest, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCreateRequest, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRequest, err)
	}
	defer resp.Body.Close()

	// Drain body to allow connection reuse.
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	default:
		return fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}
}
