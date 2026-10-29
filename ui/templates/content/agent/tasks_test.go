package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
)

func TestTaskInspectorIsolatesSwapAndDoesNotLoadCollapsed(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskRows("agent", []proxyv1.Task{{ID: "task", UID: "uid", Slot: "lane", Status: "running"}}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	if !strings.Contains(html, `hx-target="this" hx-select=".task-execution" hx-trigger="toggle[event.target.open] from:closest details, every 5s`) {
		t.Fatalf("task inspector inherits parent target or eagerly loads: %s", html)
	}
	if !strings.Contains(html, `id="`+inspectorID("agent", "uid")+`" hx-preserve`) || !strings.Contains(html, `document.getElementById(`) {
		t.Fatal("open inspector is not preserved or independently refreshed")
	}
	if !strings.Contains(html, `task_uid=uid`) {
		t.Fatal("log URL is not bound to task incarnation")
	}
	if !strings.Contains(html, `Live logs`) || !strings.Contains(html, `Task details and run history`) {
		t.Fatal("task rows missing operations")
	}
}

func TestInspectorIdentityKeepsDistinctTaskIncarnations(t *testing.T) {
	if inspectorID("agent", "A") == inspectorID("agent", "a") || inspectorID("a-b", "c") == inspectorID("a", "b-c") {
		t.Fatal("inspector identity collision")
	}
	if !strings.Contains(inspectorTrigger("agent", "uid"), inspectorID("agent", "uid")) {
		t.Fatal("polling predicate targets a different inspector")
	}
}
