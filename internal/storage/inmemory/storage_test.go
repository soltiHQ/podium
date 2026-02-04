package inmemory

import (
	"context"
	stdErrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain"
	discoverv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func newTestAgent(t *testing.T, id, platform, osName, arch string, labels map[string]string) *domain.AgentModel {
	t.Helper()

	raw := &discoverv1.SyncRequest{
		Id:            id,
		Name:          id + "-name",
		Endpoint:      "http://example.com:8080",
		UptimeSeconds: 42,
		Os:            osName,
		Arch:          arch,
		Platform:      platform,
		Metadata:      map[string]string{"meta": "value"},
	}
	a, err := domain.NewAgentModel(raw)
	if err != nil {
		t.Fatalf("NewAgentModel(%q) failed: %v", id, err)
	}

	for k, v := range labels {
		a.LabelAdd(k, v)
	}
	return a
}

func newTestUser(t *testing.T, id, subject string) *domain.UserModel {
	t.Helper()

	u, err := domain.NewUserModel(id, subject)
	if err != nil {
		t.Fatalf("NewUserModel(%q, %q) failed: %v", id, subject, err)
	}
	return u
}

func newTestCredential(t *testing.T, id, userID string, ct domain.CredentialType, data map[string]string) *domain.CredentialModel {
	t.Helper()

	c, err := domain.NewCredentialModel(id, userID, ct)
	if err != nil {
		t.Fatalf("NewCredentialModel(%q, %q) failed: %v", id, userID, err)
	}
	for k, v := range data {
		c.SetData(k, v)
	}
	return c
}

// TestStore_UpsertAndGetAgent verifies basic create and retrieve operations.
func TestStore_UpsertAndGetAgent(t *testing.T) {
	ctx := context.Background()
	s := New()

	original := newTestAgent(t, "agent-1", "kubernetes", "linux", "amd64", map[string]string{
		"env": "production",
	})

	if err := s.UpsertAgent(ctx, original); err != nil {
		t.Fatalf("UpsertAgent() failed: %v", err)
	}

	retrieved, err := s.GetAgent(ctx, "agent-1")
	if err != nil {
		t.Fatalf("GetAgent() failed: %v", err)
	}

	if retrieved == original {
		t.Error("GetAgent() should return a clone, not the original instance")
	}

	if retrieved.ID() != original.ID() {
		t.Errorf("ID mismatch: got %q, want %q", retrieved.ID(), original.ID())
	}
	if retrieved.Platform() != original.Platform() {
		t.Errorf("Platform mismatch: got %q, want %q", retrieved.Platform(), original.Platform())
	}

	origLabels := original.LabelsAll()
	retLabels := retrieved.LabelsAll()
	if len(retLabels) != len(origLabels) {
		t.Errorf("label count mismatch: got %d, want %d", len(retLabels), len(origLabels))
	}
	for k, v := range origLabels {
		if retLabels[k] != v {
			t.Errorf("label %q mismatch: got %q, want %q", k, retLabels[k], v)
		}
	}
}

// TestStore_UpsertAgent_Replace verifies that upsert replaces existing agents.
func TestStore_UpsertAgent_Replace(t *testing.T) {
	ctx := context.Background()
	s := New()

	v1 := newTestAgent(t, "agent-1", "kubernetes", "linux", "amd64", nil)
	if err := s.UpsertAgent(ctx, v1); err != nil {
		t.Fatalf("UpsertAgent(v1) failed: %v", err)
	}

	v2 := newTestAgent(t, "agent-1", "bare-metal", "darwin", "arm64", nil)
	if err := s.UpsertAgent(ctx, v2); err != nil {
		t.Fatalf("UpsertAgent(v2) failed: %v", err)
	}

	retrieved, err := s.GetAgent(ctx, "agent-1")
	if err != nil {
		t.Fatalf("GetAgent() failed: %v", err)
	}

	if retrieved.Platform() != "bare-metal" {
		t.Errorf("platform should be updated to %q, got %q", "bare-metal", retrieved.Platform())
	}
}

// TestStore_UpsertAgent_IsolatesMutations verifies that mutations don't affect stored state.
func TestStore_UpsertAgent_IsolatesMutations(t *testing.T) {
	ctx := context.Background()
	s := New()

	agent := newTestAgent(t, "agent-1", "kubernetes", "linux", "amd64", nil)
	if err := s.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("UpsertAgent() failed: %v", err)
	}

	agent.LabelAdd("mutated", "yes")

	retrieved, err := s.GetAgent(ctx, "agent-1")
	if err != nil {
		t.Fatalf("GetAgent() failed: %v", err)
	}

	if _, ok := retrieved.Label("mutated"); ok {
		t.Error("external mutation affected stored state")
	}
}

// TestStore_GetAgent_NotFound verifies proper error for missing agents.
func TestStore_GetAgent_NotFound(t *testing.T) {
	ctx := context.Background()
	s := New()

	_, err := s.GetAgent(ctx, "nonexistent")
	if err == nil {
		t.Fatal("GetAgent() should return error for missing agent")
	}
	if !stdErrors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestStore_GetAgent_EmptyID verifies rejection of empty IDs.
func TestStore_GetAgent_EmptyID(t *testing.T) {
	ctx := context.Background()
	s := New()

	_, err := s.GetAgent(ctx, "")
	if err == nil {
		t.Fatal("GetAgent(\"\") should return error")
	}
	if !stdErrors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("expected ErrInvalidArgument, got %v", err)
	}
}

// TestStore_ListAgents_EmptyStore verifies behavior with no agents.
func TestStore_ListAgents_EmptyStore(t *testing.T) {
	ctx := context.Background()
	s := New()

	result, err := s.ListAgents(ctx, nil, storage.ListOptions{})
	if err != nil {
		t.Fatalf("ListAgents() failed: %v", err)
	}

	if result.Items != nil && len(result.Items) != 0 {
		t.Errorf("expected nil or empty slice, got %d agents", len(result.Items))
	}
	if result.NextCursor != "" {
		t.Errorf("expected empty NextCursor, got %q", result.NextCursor)
	}
}

// TestStore_ListAgents_AllAgents verifies listing without filters.
func TestStore_ListAgents_AllAgents(t *testing.T) {
	ctx := context.Background()
	s := New()

	a1 := newTestAgent(t, "a1", "kubernetes", "linux", "amd64", nil)
	a2 := newTestAgent(t, "a2", "bare-metal", "darwin", "arm64", nil)

	for _, a := range []*domain.AgentModel{a1, a2} {
		if err := s.UpsertAgent(ctx, a); err != nil {
			t.Fatalf("UpsertAgent() failed: %v", err)
		}
	}

	result, err := s.ListAgents(ctx, nil, storage.ListOptions{})
	if err != nil {
		t.Fatalf("ListAgents() failed: %v", err)
	}

	if len(result.Items) != 2 {
		t.Errorf("expected 2 agents, got %d", len(result.Items))
	}
}

// TestStore_ListAgents_WithFilter verifies filtering functionality.
func TestStore_ListAgents_WithFilter(t *testing.T) {
	ctx := context.Background()
	s := New()

	agents := []*domain.AgentModel{
		newTestAgent(t, "a1", "kubernetes", "linux", "amd64", map[string]string{"env": "prod"}),
		newTestAgent(t, "a2", "kubernetes", "linux", "amd64", map[string]string{"env": "staging"}),
		newTestAgent(t, "a3", "bare-metal", "linux", "amd64", map[string]string{"env": "prod"}),
	}

	for _, a := range agents {
		if err := s.UpsertAgent(ctx, a); err != nil {
			t.Fatalf("UpsertAgent() failed: %v", err)
		}
	}

	filter := NewFilter().ByPlatform("kubernetes").ByLabel("env", "prod")

	result, err := s.ListAgents(ctx, filter, storage.ListOptions{})
	if err != nil {
		t.Fatalf("ListAgents() failed: %v", err)
	}

	if len(result.Items) != 1 {
		t.Errorf("expected 1 agent, got %d", len(result.Items))
	}
	if len(result.Items) > 0 && result.Items[0].ID() != "a1" {
		t.Errorf("expected agent a1, got %s", result.Items[0].ID())
	}
}

// TestStore_ListAgents_Pagination verifies cursor-based pagination.
func TestStore_ListAgents_Pagination(t *testing.T) {
	ctx := context.Background()
	s := New()

	for i := 0; i < 250; i++ {
		id := fmt.Sprintf("agent-%03d", i)
		a := newTestAgent(t, id, "kubernetes", "linux", "amd64", nil)
		if err := s.UpsertAgent(ctx, a); err != nil {
			t.Fatalf("UpsertAgent(%s) failed: %v", id, err)
		}
		time.Sleep(time.Millisecond)
	}

	page1, err := s.ListAgents(ctx, nil, storage.ListOptions{Limit: 100})
	if err != nil {
		t.Fatalf("ListAgents(page 1) failed: %v", err)
	}
	if len(page1.Items) != 100 {
		t.Errorf("page 1: expected 100 agents, got %d", len(page1.Items))
	}
	if page1.NextCursor == "" {
		t.Error("page 1: expected non-empty NextCursor")
	}

	page2, err := s.ListAgents(ctx, nil, storage.ListOptions{
		Cursor: page1.NextCursor,
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("ListAgents(page 2) failed: %v", err)
	}
	if len(page2.Items) != 100 {
		t.Errorf("page 2: expected 100 agents, got %d", len(page2.Items))
	}
	if page2.NextCursor == "" {
		t.Error("page 2: expected non-empty NextCursor")
	}

	page3, err := s.ListAgents(ctx, nil, storage.ListOptions{
		Cursor: page2.NextCursor,
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("ListAgents(page 3) failed: %v", err)
	}
	if len(page3.Items) != 50 {
		t.Errorf("page 3: expected 50 agents, got %d", len(page3.Items))
	}
	if page3.NextCursor != "" {
		t.Errorf("page 3: expected empty NextCursor, got %q", page3.NextCursor)
	}

	seen := make(map[string]bool)
	for _, page := range []*storage.AgentListResult{page1, page2, page3} {
		for _, a := range page.Items {
			if seen[a.ID()] {
				t.Errorf("duplicate agent across pages: %s", a.ID())
			}
			seen[a.ID()] = true
		}
	}
	if len(seen) != 250 {
		t.Errorf("expected 250 unique agents across pages, got %d", len(seen))
	}
}

// TestStore_ListAgents_LimitNormalization verifies default and max limit handling.
func TestStore_ListAgents_LimitNormalization(t *testing.T) {
	ctx := context.Background()
	s := New()

	for i := 0; i < 150; i++ {
		a := newTestAgent(t, fmt.Sprintf("a%d", i), "kubernetes", "linux", "amd64", nil)
		if err := s.UpsertAgent(ctx, a); err != nil {
			t.Fatalf("UpsertAgent() failed: %v", err)
		}
	}

	tests := []struct {
		name      string
		limit     int
		wantLimit int
	}{
		{name: "zero uses default", limit: 0, wantLimit: storage.DefaultListLimit},
		{name: "negative uses default", limit: -1, wantLimit: storage.DefaultListLimit},
		{name: "above max uses default", limit: storage.MaxListLimit + 1, wantLimit: storage.DefaultListLimit},
		{name: "valid limit preserved", limit: 50, wantLimit: 50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := s.ListAgents(ctx, nil, storage.ListOptions{Limit: tt.limit})
			if err != nil {
				t.Fatalf("ListAgents() failed: %v", err)
			}
			if len(result.Items) != tt.wantLimit {
				t.Errorf("expected exactly %d agents, got %d", tt.wantLimit, len(result.Items))
			}
		})
	}
}

// TestStore_ListAgents_InvalidCursor verifies rejection of malformed cursors.
func TestStore_ListAgents_InvalidCursor(t *testing.T) {
	ctx := context.Background()
	s := New()

	tests := []struct {
		name   string
		cursor string
	}{
		{name: "invalid base64", cursor: "not-base64!!!"},
		{name: "valid base64 invalid json", cursor: "aGVsbG8gd29ybGQ="},
		{name: "corrupted structure", cursor: "eyJpbnZhbGlkIjogdHJ1ZX0="},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.ListAgents(ctx, nil, storage.ListOptions{Cursor: tt.cursor, Limit: 10})
			if err == nil {
				t.Error("ListAgents() should reject invalid cursor")
			}
			if !stdErrors.Is(err, storage.ErrInvalidArgument) {
				t.Errorf("expected ErrInvalidArgument, got %v", err)
			}
		})
	}
}

// TestStore_ListAgents_IsolatesMutations verifies returned agents are clones.
func TestStore_ListAgents_IsolatesMutations(t *testing.T) {
	ctx := context.Background()
	s := New()

	agent := newTestAgent(t, "agent-1", "kubernetes", "linux", "amd64", nil)
	if err := s.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("UpsertAgent() failed: %v", err)
	}

	result, err := s.ListAgents(ctx, nil, storage.ListOptions{})
	if err != nil {
		t.Fatalf("ListAgents() failed: %v", err)
	}

	result.Items[0].LabelAdd("external", "mutation")

	retrieved, err := s.GetAgent(ctx, "agent-1")
	if err != nil {
		t.Fatalf("GetAgent() failed: %v", err)
	}

	if _, ok := retrieved.Label("external"); ok {
		t.Error("mutation of ListAgents result affected stored state")
	}
}

// TestStore_DeleteAgent verifies deletion functionality.
func TestStore_DeleteAgent(t *testing.T) {
	ctx := context.Background()
	s := New()

	agent := newTestAgent(t, "to-delete", "kubernetes", "linux", "amd64", nil)
	if err := s.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("UpsertAgent() failed: %v", err)
	}
	if err := s.DeleteAgent(ctx, "to-delete"); err != nil {
		t.Fatalf("DeleteAgent() failed: %v", err)
	}

	err := s.DeleteAgent(ctx, "to-delete")
	if err == nil {
		t.Fatal("DeleteAgent() should fail for already-deleted agent")
	}
	if !stdErrors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestStore_DeleteAgent_EmptyID verifies rejection of empty IDs.
func TestStore_DeleteAgent_EmptyID(t *testing.T) {
	ctx := context.Background()
	s := New()

	err := s.DeleteAgent(ctx, "")
	if err == nil {
		t.Fatal("DeleteAgent(\"\") should return error")
	}
	if !stdErrors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("expected ErrInvalidArgument, got %v", err)
	}
}

// TestStore_UpsertAndGetUser verifies basic create and retrieve operations.
func TestStore_UpsertAndGetUser(t *testing.T) {
	ctx := context.Background()
	s := New()

	original := newTestUser(t, "user-1", "sub-1")

	if err := s.UpsertUser(ctx, original); err != nil {
		t.Fatalf("UpsertUser() failed: %v", err)
	}
	retrieved, err := s.GetUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetUser() failed: %v", err)
	}
	if retrieved == original {
		t.Error("GetUser() should return a clone, not the original instance")
	}
	if retrieved.ID() != original.ID() {
		t.Errorf("ID mismatch: got %q, want %q", retrieved.ID(), original.ID())
	}
	if retrieved.Subject() != original.Subject() {
		t.Errorf("Subject mismatch: got %q, want %q", retrieved.Subject(), original.Subject())
	}
}

// TestStore_GetUser_NotFound verifies proper error for missing users.
func TestStore_GetUser_NotFound(t *testing.T) {
	ctx := context.Background()
	s := New()

	_, err := s.GetUser(ctx, "nonexistent")
	if err == nil {
		t.Fatal("GetUser() should return error for missing user")
	}
	if !stdErrors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestStore_GetUser_EmptyID verifies rejection of empty IDs.
func TestStore_GetUser_EmptyID(t *testing.T) {
	ctx := context.Background()
	s := New()

	_, err := s.GetUser(ctx, "")
	if err == nil {
		t.Fatal("GetUser(\"\") should return error")
	}
	if !stdErrors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("expected ErrInvalidArgument, got %v", err)
	}
}

// TestStore_GetUserBySubject verifies subject-based retrieval.
func TestStore_GetUserBySubject(t *testing.T) {
	ctx := context.Background()
	s := New()

	u1 := newTestUser(t, "user-1", "subject-1")
	u2 := newTestUser(t, "user-2", "subject-2")

	for _, u := range []*domain.UserModel{u1, u2} {
		if err := s.UpsertUser(ctx, u); err != nil {
			t.Fatalf("UpsertUser() failed: %v", err)
		}
	}
	retrieved, err := s.GetUserBySubject(ctx, "subject-1")
	if err != nil {
		t.Fatalf("GetUserBySubject() failed: %v", err)
	}
	if retrieved.ID() != "user-1" {
		t.Errorf("expected user-1, got %s", retrieved.ID())
	}
	if retrieved.Subject() != "subject-1" {
		t.Errorf("expected subject-1, got %s", retrieved.Subject())
	}
}

// TestStore_GetUserBySubject_NotFound verifies proper error for missing subject.
func TestStore_GetUserBySubject_NotFound(t *testing.T) {
	ctx := context.Background()
	s := New()

	_, err := s.GetUserBySubject(ctx, "nonexistent-subject")
	if err == nil {
		t.Fatal("GetUserBySubject() should return error for missing subject")
	}
	if !stdErrors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestStore_GetUserBySubject_EmptySubject verifies rejection of empty subject.
func TestStore_GetUserBySubject_EmptySubject(t *testing.T) {
	ctx := context.Background()
	s := New()

	_, err := s.GetUserBySubject(ctx, "")
	if err == nil {
		t.Fatal("GetUserBySubject(\"\") should return error")
	}
	if !stdErrors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("expected ErrInvalidArgument, got %v", err)
	}
}

// TestStore_ListUsers_EmptyStore verifies behavior with no users.
func TestStore_ListUsers_EmptyStore(t *testing.T) {
	ctx := context.Background()
	s := New()

	result, err := s.ListUsers(ctx, nil, storage.ListOptions{})
	if err != nil {
		t.Fatalf("ListUsers() failed: %v", err)
	}
	if result.Items != nil && len(result.Items) != 0 {
		t.Errorf("expected nil or empty slice, got %d users", len(result.Items))
	}
	if result.NextCursor != "" {
		t.Errorf("expected empty NextCursor, got %q", result.NextCursor)
	}
}

// TestStore_ListUsers_AllUsers verifies listing without filters.
func TestStore_ListUsers_AllUsers(t *testing.T) {
	ctx := context.Background()
	s := New()

	u1 := newTestUser(t, "user-1", "sub-1")
	u2 := newTestUser(t, "user-2", "sub-2")

	for _, u := range []*domain.UserModel{u1, u2} {
		if err := s.UpsertUser(ctx, u); err != nil {
			t.Fatalf("UpsertUser() failed: %v", err)
		}
	}
	result, err := s.ListUsers(ctx, nil, storage.ListOptions{})
	if err != nil {
		t.Fatalf("ListUsers() failed: %v", err)
	}
	if len(result.Items) != 2 {
		t.Errorf("expected 2 users, got %d", len(result.Items))
	}
}

// TestStore_ListUsers_Pagination verifies cursor-based pagination.
func TestStore_ListUsers_Pagination(t *testing.T) {
	ctx := context.Background()
	s := New()

	for i := 0; i < 250; i++ {
		id := fmt.Sprintf("user-%03d", i)
		subject := fmt.Sprintf("sub-%03d", i)
		u := newTestUser(t, id, subject)
		if err := s.UpsertUser(ctx, u); err != nil {
			t.Fatalf("UpsertUser(%s) failed: %v", id, err)
		}
		time.Sleep(time.Millisecond)
	}

	page1, err := s.ListUsers(ctx, nil, storage.ListOptions{Limit: 100})
	if err != nil {
		t.Fatalf("ListUsers(page 1) failed: %v", err)
	}
	if len(page1.Items) != 100 {
		t.Errorf("page 1: expected 100 users, got %d", len(page1.Items))
	}
	if page1.NextCursor == "" {
		t.Error("page 1: expected non-empty NextCursor")
	}
	page2, err := s.ListUsers(ctx, nil, storage.ListOptions{
		Cursor: page1.NextCursor,
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("ListUsers(page 2) failed: %v", err)
	}
	if len(page2.Items) != 100 {
		t.Errorf("page 2: expected 100 users, got %d", len(page2.Items))
	}
	if page2.NextCursor == "" {
		t.Error("page 2: expected non-empty NextCursor")
	}
	page3, err := s.ListUsers(ctx, nil, storage.ListOptions{
		Cursor: page2.NextCursor,
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("ListUsers(page 3) failed: %v", err)
	}
	if len(page3.Items) != 50 {
		t.Errorf("page 3: expected 50 users, got %d", len(page3.Items))
	}
	if page3.NextCursor != "" {
		t.Errorf("page 3: expected empty NextCursor, got %q", page3.NextCursor)
	}
	seen := make(map[string]bool)
	for _, page := range []*storage.UserListResult{page1, page2, page3} {
		for _, u := range page.Items {
			if seen[u.ID()] {
				t.Errorf("duplicate user across pages: %s", u.ID())
			}
			seen[u.ID()] = true
		}
	}
	if len(seen) != 250 {
		t.Errorf("expected 250 unique users across pages, got %d", len(seen))
	}
}

// TestStore_ListUsers_LimitNormalization verifies default and max limit handling.
func TestStore_ListUsers_LimitNormalization(t *testing.T) {
	ctx := context.Background()
	s := New()

	for i := 0; i < 150; i++ {
		u := newTestUser(t, fmt.Sprintf("u%d", i), fmt.Sprintf("sub%d", i))
		if err := s.UpsertUser(ctx, u); err != nil {
			t.Fatalf("UpsertUser() failed: %v", err)
		}
	}

	tests := []struct {
		name      string
		limit     int
		wantLimit int
	}{
		{name: "zero uses default", limit: 0, wantLimit: storage.DefaultListLimit},
		{name: "negative uses default", limit: -1, wantLimit: storage.DefaultListLimit},
		{name: "above max uses default", limit: storage.MaxListLimit + 1, wantLimit: storage.DefaultListLimit},
		{name: "valid limit preserved", limit: 50, wantLimit: 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := s.ListUsers(ctx, nil, storage.ListOptions{Limit: tt.limit})
			if err != nil {
				t.Fatalf("ListUsers() failed: %v", err)
			}
			if len(result.Items) != tt.wantLimit {
				t.Errorf("expected exactly %d users, got %d", tt.wantLimit, len(result.Items))
			}
		})
	}
}

// TestStore_ListUsers_InvalidCursor verifies rejection of malformed cursors.
func TestStore_ListUsers_InvalidCursor(t *testing.T) {
	ctx := context.Background()
	s := New()

	tests := []struct {
		name   string
		cursor string
	}{
		{name: "invalid base64", cursor: "not-base64!!!"},
		{name: "valid base64 invalid json", cursor: "aGVsbG8gd29ybGQ="},
		{name: "corrupted structure", cursor: "eyJpbnZhbGlkIjogdHJ1ZX0="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.ListUsers(ctx, nil, storage.ListOptions{Cursor: tt.cursor, Limit: 10})
			if err == nil {
				t.Error("ListUsers() should reject invalid cursor")
			}
			if !stdErrors.Is(err, storage.ErrInvalidArgument) {
				t.Errorf("expected ErrInvalidArgument, got %v", err)
			}
		})
	}
}

// TestStore_DeleteUser verifies deletion functionality.
func TestStore_DeleteUser(t *testing.T) {
	ctx := context.Background()
	s := New()

	user := newTestUser(t, "to-delete", "subject-delete")
	if err := s.UpsertUser(ctx, user); err != nil {
		t.Fatalf("UpsertUser() failed: %v", err)
	}
	if err := s.DeleteUser(ctx, "to-delete"); err != nil {
		t.Fatalf("DeleteUser() failed: %v", err)
	}
	err := s.DeleteUser(ctx, "to-delete")
	if err == nil {
		t.Fatal("DeleteUser() should fail for already-deleted user")
	}
	if !stdErrors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestStore_DeleteUser_EmptyID verifies rejection of empty IDs.
func TestStore_DeleteUser_EmptyID(t *testing.T) {
	ctx := context.Background()
	s := New()

	err := s.DeleteUser(ctx, "")
	if err == nil {
		t.Fatal("DeleteUser(\"\") should return error")
	}
	if !stdErrors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("expected ErrInvalidArgument, got %v", err)
	}
}

// TestStore_UpsertAndGetCredential tests the upsert and retrieval functionality of credentials in the store.
func TestStore_UpsertAndGetCredential(t *testing.T) {
	ctx := context.Background()
	s := New()

	original := newTestCredential(t, "cred-1", "user-1", domain.CredentialTypePassword, map[string]string{
		"hash": "h1",
		"salt": "s1",
	})
	if err := s.UpsertCredential(ctx, original); err != nil {
		t.Fatalf("UpsertCredential() failed: %v", err)
	}

	retrieved, err := s.GetCredential(ctx, "cred-1")
	if err != nil {
		t.Fatalf("GetCredential() failed: %v", err)
	}
	if retrieved == original {
		t.Error("GetCredential() should return a clone, not the original instance")
	}
	if retrieved.ID() != original.ID() {
		t.Errorf("ID mismatch: got %q, want %q", retrieved.ID(), original.ID())
	}
	if retrieved.UserID() != original.UserID() {
		t.Errorf("UserID mismatch: got %q, want %q", retrieved.UserID(), original.UserID())
	}
	if retrieved.Type() != original.Type() {
		t.Errorf("Type mismatch: got %v, want %v", retrieved.Type(), original.Type())
	}
	if v, ok := retrieved.GetData("hash"); !ok || v != "h1" {
		t.Fatalf("expected data hash=h1, got %q (ok=%v)", v, ok)
	}
}

// TestStore_UpsertCredential_Nil ensures UpsertCredential does not accept a nil argument and returns ErrInvalidArgument.
func TestStore_UpsertCredential_Nil(t *testing.T) {
	ctx := context.Background()
	s := New()

	err := s.UpsertCredential(ctx, nil)
	if err == nil {
		t.Fatal("UpsertCredential(nil) should return error")
	}
	if !stdErrors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}

// TestStore_GetCredential_NotFound tests that GetCredential returns an error when the requested credential does not exist.
func TestStore_GetCredential_NotFound(t *testing.T) {
	ctx := context.Background()
	s := New()

	_, err := s.GetCredential(ctx, "nope")
	if err == nil {
		t.Fatal("GetCredential() should return error for missing credential")
	}
	if !stdErrors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestStore_GetCredential_EmptyID validates that GetCredential returns an error when called with an empty ID.
func TestStore_GetCredential_EmptyID(t *testing.T) {
	ctx := context.Background()
	s := New()

	_, err := s.GetCredential(ctx, "")
	if err == nil {
		t.Fatal(`GetCredential("") should return error`)
	}
	if !stdErrors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}

// TestStore_UpsertCredential_IsolatesMutations verifies that UpsertCredential creates an isolated copy, ignoring external mutations.
func TestStore_UpsertCredential_IsolatesMutations(t *testing.T) {
	ctx := context.Background()
	s := New()

	cred := newTestCredential(t, "cred-1", "user-1", domain.CredentialTypePassword, map[string]string{
		"hash": "h1",
	})
	if err := s.UpsertCredential(ctx, cred); err != nil {
		t.Fatalf("UpsertCredential() failed: %v", err)
	}

	cred.SetData("hash", "mutated")
	got, err := s.GetCredential(ctx, "cred-1")
	if err != nil {
		t.Fatalf("GetCredential() failed: %v", err)
	}
	v, ok := got.GetData("hash")
	if !ok || v != "h1" {
		t.Fatalf("expected stored hash=h1, got %q (ok=%v)", v, ok)
	}
}

// TestStore_GetCredentialByUserAndType verifies retrieval of a credential by user ID and credential type from the store.
func TestStore_GetCredentialByUserAndType(t *testing.T) {
	ctx := context.Background()
	s := New()
	
	c1 := newTestCredential(t, "c1", "user-1", domain.CredentialTypePassword, map[string]string{"hash": "h"})
	c2 := newTestCredential(t, "c2", "user-1", domain.CredentialTypeAPIKey, map[string]string{"sub": "x"})
	c3 := newTestCredential(t, "c3", "user-2", domain.CredentialTypePassword, map[string]string{"hash": "h2"})

	for _, c := range []*domain.CredentialModel{c1, c2, c3} {
		if err := s.UpsertCredential(ctx, c); err != nil {
			t.Fatalf("UpsertCredential() failed: %v", err)
		}
	}

	got, err := s.GetCredentialByUserAndType(ctx, "user-1", domain.CredentialTypeAPIKey)
	if err != nil {
		t.Fatalf("GetCredentialByUserAndType() failed: %v", err)
	}
	if got.ID() != "c2" {
		t.Fatalf("expected c2, got %s", got.ID())
	}
	if got.UserID() != "user-1" {
		t.Fatalf("expected user-1, got %s", got.UserID())
	}
	if got.Type() != domain.CredentialTypeAPIKey {
		t.Fatalf("expected OIDC, got %v", got.Type())
	}
}

// TestStore_GetCredentialByUserAndType_EmptyUserID verifies that GetCredentialByUserAndType rejects an empty userID with an error.
func TestStore_GetCredentialByUserAndType_EmptyUserID(t *testing.T) {
	ctx := context.Background()
	s := New()

	_, err := s.GetCredentialByUserAndType(ctx, "", domain.CredentialTypePassword)
	if err == nil {
		t.Fatal("GetCredentialByUserAndType() should reject empty userID")
	}
	if !stdErrors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}

// TestStore_GetCredentialByUserAndType_NotFound verifies behavior when fetching a credential by user and type that does not exist.
func TestStore_GetCredentialByUserAndType_NotFound(t *testing.T) {
	ctx := context.Background()
	s := New()

	c := newTestCredential(t, "c1", "user-1", domain.CredentialTypePassword, map[string]string{"hash": "h"})
	if err := s.UpsertCredential(ctx, c); err != nil {
		t.Fatalf("UpsertCredential() failed: %v", err)
	}

	_, err := s.GetCredentialByUserAndType(ctx, "user-1", domain.CredentialTypeAPIKey)
	if err == nil {
		t.Fatal("expected error for missing credential type")
	}
	if !stdErrors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestStore_ListCredentialsByUser verifies that the ListCredentialsByUser function retrieves all credentials for a given user ID.
func TestStore_ListCredentialsByUser(t *testing.T) {
	ctx := context.Background()
	s := New()

	c1 := newTestCredential(t, "c1", "user-1", domain.CredentialTypePassword, map[string]string{"hash": "h"})
	c2 := newTestCredential(t, "c2", "user-1", domain.CredentialTypeAPIKey, map[string]string{"sub": "x"})
	c3 := newTestCredential(t, "c3", "user-2", domain.CredentialTypePassword, map[string]string{"hash": "h2"})

	for _, c := range []*domain.CredentialModel{c1, c2, c3} {
		if err := s.UpsertCredential(ctx, c); err != nil {
			t.Fatalf("UpsertCredential() failed: %v", err)
		}
	}

	items, err := s.ListCredentialsByUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("ListCredentialsByUser() failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 credentials, got %d", len(items))
	}
	for _, it := range items {
		if it.UserID() != "user-1" {
			t.Fatalf("unexpected credential userID: %s", it.UserID())
		}
	}
}

// TestStore_ListCredentialsByUser_EmptyUserID tests that ListCredentialsByUser returns an error when given an empty userID.
func TestStore_ListCredentialsByUser_EmptyUserID(t *testing.T) {
	ctx := context.Background()
	s := New()

	_, err := s.ListCredentialsByUser(ctx, "")
	if err == nil {
		t.Fatal("ListCredentialsByUser() should reject empty userID")
	}
	if !stdErrors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}

// TestStore_DeleteCredential verifies the deletion of a credential, including the handling of non-existent credentials.
func TestStore_DeleteCredential(t *testing.T) {
	ctx := context.Background()
	s := New()

	c := newTestCredential(t, "cred-del", "user-1", domain.CredentialTypePassword, map[string]string{"hash": "h"})
	if err := s.UpsertCredential(ctx, c); err != nil {
		t.Fatalf("UpsertCredential() failed: %v", err)
	}

	if err := s.DeleteCredential(ctx, "cred-del"); err != nil {
		t.Fatalf("DeleteCredential() failed: %v", err)
	}

	err := s.DeleteCredential(ctx, "cred-del")
	if err == nil {
		t.Fatal("DeleteCredential() should fail for already-deleted credential")
	}
	if !stdErrors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestStore_DeleteCredential_EmptyID tests the behavior of DeleteCredential when called with an empty ID.
func TestStore_DeleteCredential_EmptyID(t *testing.T) {
	ctx := context.Background()
	s := New()

	err := s.DeleteCredential(ctx, "")
	if err == nil {
		t.Fatal(`DeleteCredential("") should return error`)
	}
	if !stdErrors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}
