package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

// httpProxyV1.ApplyTask must return the TaskId from ApplyTaskResponse.
// Without it CP cannot later DeleteTask or GetTaskStatus for this exact
// run, which breaks the uninstall (delete-on-target-removal) flow.
func TestHttpProxyV1_ApplyTask_ReturnsTaskID(t *testing.T) {
	want := "my-task"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/apis/solti.io/v1/tasks/my-task") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		// Drain so we don't trip test server's internal assertions.
		_, _ = io.Copy(io.Discard, r.Body)

		body, err := json.Marshal(map[string]any{"metadata": map[string]any{"name": want, "uid": "uid-1"}})
		if err != nil {
			t.Fatalf("marshal response: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	p := &httpProxyV1{endpoint: srv.URL, client: srv.Client()}

	got, err := p.ApplyTask(context.Background(), TaskSubmission{
		Spec: httpTestManifest(t, "my-task"),
	})
	if err != nil {
		t.Fatalf("ApplyTask: %v", err)
	}
	if got != want {
		t.Errorf("task id: got %q, want %q", got, want)
	}
}

// An empty task_id is a protocol violation — the agent accepted the
// apply but gave us no handle to manage the run. Fail loudly rather
// than pretend-it-is-synced.
func TestHttpProxyV1_ApplyTask_RejectsEmptyTaskID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": ""}})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	p := &httpProxyV1{endpoint: srv.URL, client: srv.Client()}

	_, err := p.ApplyTask(context.Background(), TaskSubmission{Spec: httpTestManifest(t, "task")})
	if err == nil {
		t.Fatal("expected error for empty task_id")
	}
	if !errors.Is(err, ErrApplyTask) {
		t.Errorf("error should wrap ErrApplyTask, got %v", err)
	}
	if !strings.Contains(err.Error(), "empty task id") {
		t.Errorf("error should mention empty task id, got %q", err.Error())
	}
}

// When the agent rejects the spec (400 with SDK error envelope), the
// error surfaced by the proxy must contain both the label and message.
// Verified earlier via formatUnexpectedStatus; this test keeps the
// end-to-end wiring covered when the decoding helper is on the hot path.
func TestHttpProxyV1_ApplyTask_Surfaces400Envelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"InvalidRequest","message":"slot cannot be empty"}`))
	}))
	defer srv.Close()

	p := &httpProxyV1{endpoint: srv.URL, client: srv.Client()}

	_, err := p.ApplyTask(context.Background(), TaskSubmission{Spec: httpTestManifest(t, "task")})
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if !strings.Contains(err.Error(), "InvalidRequest") || !strings.Contains(err.Error(), "slot cannot be empty") {
		t.Errorf("error should carry SDK envelope, got %q", err.Error())
	}
}

// An unparsable endpoint must fail the stream open, never panic in the log hub goroutine.
func TestStreamLogsInvalidEndpointReturnsError(t *testing.T) {
	p := &httpProxyV1{endpoint: "http://[", client: http.DefaultClient}
	if _, err := p.StreamTaskLogsForUID(context.Background(), "task", "uid"); !errors.Is(err, ErrBadEndpointURL) {
		t.Fatalf("StreamTaskLogsForUID = %v, want ErrBadEndpointURL", err)
	}
}

func TestApplyTaskPreconditionsInvalidEndpointReturnsError(t *testing.T) {
	uid := "uid-1"
	p := &httpProxyV1{endpoint: "http://[", client: http.DefaultClient}
	_, err := p.ApplyTask(context.Background(), TaskSubmission{Spec: httpTestManifest(t, "task"), Preconditions: &taskv1.WritePreconditions{Uid: &uid}})
	if !errors.Is(err, ErrBadEndpointURL) {
		t.Fatalf("ApplyTask = %v, want ErrBadEndpointURL", err)
	}
}

func httpTestManifest(t *testing.T, name string) *taskv1.TaskManifest {
	t.Helper()
	s, _ := model.NewSpec("test", "test", "my-slot")
	s.SetKindConfig(map[string]any{"command": map[string]any{"command": "echo"}})
	m, err := SpecToProto(s)
	if err != nil {
		t.Fatal(err)
	}
	m.Metadata.Name = name
	return m
}

// A 2xx body that the deadline cuts off, or that is not JSON, keeps its cause
// visible to errors.Is and errors.As: the sync runner tells an agent that stopped
// answering from one that answered badly.
func TestHttpProxyV1_Request_KeepsBodyErrorCauses(t *testing.T) {
	t.Run("body cut by the deadline", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"metadata":`))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer srv.Close()
		defer close(release)
		p := &httpProxyV1{endpoint: srv.URL, client: srv.Client()}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		_, err := p.GetTask(ctx, "my-task")
		if !errors.Is(err, ErrDecode) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("GetTask = %v, want a decode error that is context.DeadlineExceeded", err)
		}
	})
	t.Run("malformed body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"metadata":`))
		}))
		defer srv.Close()
		p := &httpProxyV1{endpoint: srv.URL, client: srv.Client()}

		_, err := p.GetTask(context.Background(), "my-task")
		var syntaxErr *json.SyntaxError
		if !errors.Is(err, ErrDecode) || !errors.As(err, &syntaxErr) {
			t.Fatalf("GetTask = %v, want a decode error carrying the JSON syntax error", err)
		}
	})
}

// A body over the cap fails as a decode error that says it was too large, so that
// a caller can ask for less. A malformed body does not say it.
func TestHttpProxyV1_ListTasks_TagsABodyOverTheCap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   func(w io.Writer)
		tagged bool
	}{
		{"body over the cap", func(w io.Writer) {
			_, _ = w.Write([]byte(`{"items":[],"pad":"`))
			_, _ = w.Write(bytes.Repeat([]byte("a"), maxAgentResponseBytes))
			_, _ = w.Write([]byte(`"}`))
		}, true},
		{"malformed body", func(w io.Writer) { _, _ = w.Write([]byte(`{"items":`)) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				tc.body(w)
			}))
			defer srv.Close()
			p := &httpProxyV1{endpoint: srv.URL, client: srv.Client()}

			_, err := p.ListTasks(context.Background(), TaskFilter{Limit: 1000})

			if !errors.Is(err, ErrListTasks) || !errors.Is(err, ErrDecode) {
				t.Fatalf("ListTasks = %v, want a list tasks decode error", err)
			}
			if got := errors.Is(err, ErrResponseTooLarge); got != tc.tagged {
				t.Fatalf("ErrResponseTooLarge = %v, want %v: %v", got, tc.tagged, err)
			}
			if kind := errkind.Classify(err); kind != errkind.Internal {
				t.Fatalf("kind = %v, want Internal: tagging must not change the classification", kind)
			}
		})
	}
}
