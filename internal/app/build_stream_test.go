package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/cluster/standalone"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// Exercise the composition root, including persisted-session authorization and
// real HTTP cancellation. Events and task logs must share the same IP budget.
func TestAuthenticatedEventAndTaskLogStreamsShareLimitAndRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logger := zerolog.Nop()
	cfg := config.Default()
	cfg.Auth.JWTSecret = strings.Repeat("stream-test-only-secret", 2)
	cfg.Auth.RateAttempts = 1000 // Separate admission pressure from login failures.
	cfg.Auth = cfg.Auth.WithDefaults()
	cfg.Streams.MaxPerIP = 2
	cfg.Streams.SubscriberBuffer = 4
	store := inmemory.New()
	authModel := kit.New(store, cfg.Auth)
	svc, err := initServices(store, authModel, cfg.History.Archive, logger)
	if err != nil {
		t.Fatal(err)
	}
	tokenFor := func(userID string, permissions []string) string {
		t.Helper()
		user, err := model.NewUser(userID, userID)
		if err != nil {
			t.Fatal(err)
		}
		user.Enable()
		user.SetPermissions(permissions)
		if err := store.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		sessionID := "session-" + userID
		session, err := model.NewSession(sessionID, userID, "credential", enum.Password, make([]byte, 32), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
		value, err := jwt.NewHSIssuer([]byte(cfg.Auth.JWTSecret), authModel.Clock).Issue(ctx, &identity.Identity{
			UserID: userID, Subject: userID, SessionID: sessionID, TokenID: "token-" + userID, Issuer: cfg.Auth.Issuer,
			Audience: []string{cfg.Auth.Audience}, ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	allowed := tokenFor("allowed", []string{string(enum.AgentsGet)})
	noPermission := tokenFor("no-permission", []string{string(enum.SpecsGet)})
	sourceCanceled := make(chan struct{}, 1)
	agentHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/solti.io/v1/tasks/task/logs" || r.URL.Query().Get("taskUid") != "original-uid" {
			http.Error(w, "unexpected task identity", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		sourceCanceled <- struct{}{}
	}))
	defer agentHTTP.Close()
	ag, err := model.NewAgentFrom(model.AgentParams{ID: "fixture-agent", Name: "fixture", Endpoint: agentHTTP.URL, EndpointType: 2, APIVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	pool := proxy.NewPool(nil, cfg.AgentAuth.Require)
	defer func() { _ = pool.Close() }()
	events := event.NewHub(logger)
	defer events.Close()
	handler, logs := buildMainHandler(cfg, logger, svc, authModel, pool, events, standalone.NewLeadership())
	defer logs.Close()
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: time.Second}}
	defer client.CloseIdleConnections()
	const eventPath = "/api/v1/events/stream"
	const logPath = "/api/v1/agents/fixture-agent/tasks/task/logs/stream?task_uid=original-uid"
	type held struct {
		response *http.Response
		cancel   context.CancelFunc
	}
	var heldStreams []*held
	defer func() {
		for _, stream := range heldStreams {
			stream.cancel()
			_ = stream.response.Body.Close()
		}
	}()
	open := func(path, token string) *held {
		t.Helper()
		requestCtx, requestCancel := context.WithCancel(ctx)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Accept", "text/event-stream")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(request)
		if err != nil {
			requestCancel()
			t.Fatal(err)
		}
		stream := &held{response: response, cancel: requestCancel}
		if response.StatusCode == http.StatusOK {
			if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatal("stream did not return SSE")
			}
			heldStreams = append(heldStreams, stream)
		} else {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
			_ = response.Body.Close()
			requestCancel()
		}
		return stream
	}
	expect := func(path, token string, status int) *held {
		t.Helper()
		stream := open(path, token)
		if stream.response.StatusCode != status {
			t.Fatalf("%s: got HTTP%d, want%d", path, stream.response.StatusCode, status)
		}
		return stream
	}
	reopen := func(path string) *held {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			stream := open(path, allowed)
			if stream.response.StatusCode == http.StatusOK {
				return stream
			}
			if stream.response.StatusCode != http.StatusTooManyRequests || time.Now().After(deadline) {
				t.Fatalf("slot not released: HTTP%d", stream.response.StatusCode)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	first := expect(eventPath, allowed, 200)
	second := expect(eventPath, allowed, 200)
	expect(eventPath, allowed, 429)
	expect(logPath, allowed, 429)
	expect(eventPath, "", 401)         // Authentication precedes slot admission.
	expect(logPath, noPermission, 403) // Log permission precedes slot admission.
	first.cancel()
	_ = first.response.Body.Close()
	logStream := reopen(logPath) // One event + one task log now occupy both slots.
	expect(eventPath, allowed, 429)
	expect(logPath, allowed, 429)
	logStream.cancel()
	_ = logStream.response.Body.Close()
	select {
	case <-sourceCanceled:
	case <-ctx.Done():
		t.Fatal("last log viewer did not cancel the backing agent stream")
	}
	third := reopen(eventPath)
	expect(logPath, allowed, 429)
	second.cancel()
	_ = second.response.Body.Close()
	third.cancel()
	_ = third.response.Body.Close()
	fourth := reopen(eventPath)
	fifth := reopen(eventPath)
	expect(eventPath, allowed, 429)
	fourth.cancel()
	_ = fourth.response.Body.Close()
	fifth.cancel()
	_ = fifth.response.Body.Close()
	t.Log("shared cap2, authenticated/permission ordering, mixed events+logs, backing cancellation, and release/reopen verified")
}
