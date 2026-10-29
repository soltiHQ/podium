package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCurrentHTTPManifestShape(t *testing.T) {
	m := httpTestManifest(t, "task")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != v1PathTasks+"/task" {
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["apiVersion"] != "solti.io/v1" || body["kind"] != "Task" {
			t.Errorf("bad manifest %v", body)
		}
		if _, ok := body["spec"].(map[string]any)["timeout"].(float64); !ok {
			t.Error("SDK requires numeric timeout")
		}
		_, _ = fmt.Fprint(w, `{"metadata":{"name":"task","uid":"uid-1"}}`)
	}))
	defer srv.Close()
	p := &httpProxyV1{endpoint: srv.URL, client: srv.Client()}
	if _, err := p.ApplyTask(context.Background(), TaskSubmission{Spec: m}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPDeletePinsUID(t *testing.T) {
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = fmt.Fprint(w, `{"metadata":{"name":"task","uid":"incarnation-a","creationTimestamp":"2026-10-01T00:00:00Z"}}`)
			return
		}
		if r.Method != "DELETE" || r.URL.Query().Get("uid") != "incarnation-a" {
			t.Error("delete lacks incarnation precondition")
		}
		deleted = true
		w.WriteHeader(204)
	}))
	defer srv.Close()
	p := &httpProxyV1{endpoint: srv.URL, client: srv.Client()}
	if err := p.DeleteTask(context.Background(), "task"); err != nil || !deleted {
		t.Fatalf("delete: %v", err)
	}
}

func TestHTTPTaskSnapshotAndRunPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/runs") {
			if r.URL.Query().Get("continue") == "" {
				_, _ = fmt.Fprint(w, `{"metadata":{"taskUid":"u","resourceVersion":"run-rv","continue":"next"},"runs":[{"generation":1,"attempt":1,"phase":"succeeded","startedAt":"2026-10-01T00:00:00Z"}]}`)
			} else {
				_, _ = fmt.Fprint(w, `{"metadata":{"taskUid":"u","resourceVersion":"run-rv"},"runs":[{"generation":2,"attempt":1,"phase":"running","startedAt":"2026-10-01T00:00:01Z"}]}`)
			}
			return
		}
		if r.URL.Query().Get("continue") != "opaque" || r.URL.Query().Get("phase") != "running" {
			t.Error("wrong filters")
		}
		_, _ = fmt.Fprint(w, `{"metadata":{"resourceVersion":"rv","continue":"next","remainingItemCount":3},"items":[]}`)
	}))
	defer srv.Close()
	p := &httpProxyV1{endpoint: srv.URL, client: srv.Client()}
	page, err := p.ListTasks(context.Background(), TaskFilter{Cursor: "opaque", Status: "running"})
	if err != nil || page.NextCursor != "next" || page.RemainingCount == nil || *page.RemainingCount != 3 {
		t.Fatalf("snapshot %+v %v", page, err)
	}
	runs, err := p.ListTaskRuns(context.Background(), "task")
	if err != nil || len(runs.Runs) != 2 || runs.Runs[1].Generation != 2 {
		t.Fatalf("runs %+v %v", runs, err)
	}
}

func TestSSEBinaryGenerationUIDAndDiscriminator(t *testing.T) {
	payload := `{"type":"chunk","taskUid":"u","generation":3,"attempt":2,"stream":"stdout","seq":7,"ts":123,"line":"AP9h","truncated":true}`
	ev := parseAgentLogFrame("chunk", payload, "u")
	if ev == nil || ev.GetChunk().Generation != 3 || string(ev.GetChunk().Line) != string([]byte{0, 255, 'a'}) || !ev.GetChunk().Truncated {
		t.Fatalf("chunk lost %+v", ev)
	}
	if parseAgentLogFrame("run-started", payload, "u") != nil || parseAgentLogFrame("chunk", payload, "wrong") != nil {
		t.Fatal("mismatched identity accepted")
	}
}

func TestTaskFilterRejectsInvalidPaginationBeforeTransport(t *testing.T) {
	for _, limit := range []int{-1, 1001, 1 << 32} {
		f := TaskFilter{Limit: limit}
		if err := validateTaskFilter(f); err == nil {
			t.Fatalf("invalid limit %d accepted", limit)
		}
	}
	for _, f := range []TaskFilter{{Offset: 1}, {Offset: -1}, {Status: "counterfeit"}} {
		if err := validateTaskFilter(f); err == nil {
			t.Fatalf("bad filter accepted: %+v", f)
		}
	}
}
