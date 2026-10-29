package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

func TestDiscoveryAcceptsSDKProtoJSONCapabilitiesAndRejectsOversize(t *testing.T) {
	store := inmemory.New()
	logger := zerolog.Nop()
	svc := agent.New(store, logger)
	hub := event.NewHub(logger)
	handler := NewHTTPDiscovery(logger, svc, hub, false)
	body := `{"id":"sdk-agent","name":"SDK Agent","endpoint":"http://127.0.0.1:1234","uptimeSeconds":"5","ts":"100","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1,"heartbeatIntervalS":30,"capabilities":{"runners":[{"name":"default","labels":{"zone":"eu"},"workloads":[{"apiVersion":"solti.io/v1","kind":"Subprocess"}]}]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	handler.Sync(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("SDK discovery rejected: %d %s", res.Code, res.Body.String())
	}
	found, err := svc.Get(context.Background(), "sdk-agent")
	if err != nil {
		t.Fatal(err)
	}
	caps := found.Capabilities()
	if len(caps) != 1 || caps[0].Workloads[0].Group != "solti.io" || caps[0].Workloads[0].Kind != "Subprocess" || caps[0].Labels["zone"] != "eu" {
		t.Fatalf("capabilities lost: %+v", caps)
	}
	// A valid JSON prefix padded past the ceiling must never be accepted.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body+strings.Repeat(" ", maxSyncBodyBytes)))
	res = httptest.NewRecorder()
	handler.Sync(res, req)
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize valid prefix accepted: %d", res.Code)
	}
}
