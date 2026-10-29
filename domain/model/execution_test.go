package model

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func executionParams() ExecutionRecordParams {
	return ExecutionRecordParams{AgentID: "a", AgentName: "Agent", TaskName: "task", TaskUID: "uid", Generation: math.MaxUint64, Attempt: math.MaxUint32, Status: "timeout", StartedAtMs: 2000, FinishedAtMs: 1000, Error: "stopped", WorkloadAPIVersion: "solti.io/v1", WorkloadKind: "Subprocess", CapturedAt: time.Unix(123, 456)}
}

func TestExecutionIdentityBoundsAndOwnedData(t *testing.T) {
	p := executionParams()
	code := int32(-9)
	p.ExitCode = &code
	e, err := NewExecutionRecord(p)
	if err != nil {
		t.Fatal(err)
	}
	code = 77
	if *e.ExitCode() != -9 {
		t.Fatal("input exitcode aliases archive")
	}
	copy := e.Params()
	*copy.ExitCode = 42
	clone := e.Clone()
	clone.params.ExitCode = new(int32)
	if *e.ExitCode() != -9 {
		t.Fatal("returned params aliases archive")
	}
	replay := p
	replay.ExitCode = nil
	replay.CapturedAt = replay.CapturedAt.Add(time.Hour)
	replay.AgentName = "Renamed"
	r, err := NewExecutionRecord(replay)
	if err != nil || r.ID() != e.ID() {
		t.Fatal("nonidentity metadata changes identity", err)
	}
	for _, change := range []func(*ExecutionRecordParams){func(x *ExecutionRecordParams) { x.AgentID += "x" }, func(x *ExecutionRecordParams) { x.TaskName += "x" }, func(x *ExecutionRecordParams) { x.TaskUID += "x" }, func(x *ExecutionRecordParams) { x.Generation-- }, func(x *ExecutionRecordParams) { x.Attempt-- }} {
		next := p
		change(&next)
		r, err := NewExecutionRecord(next)
		if err != nil || r.ID() == e.ID() {
			t.Fatal("distinct run conflated", err)
		}
	}
	a := p
	a.AgentID = "a-b"
	a.TaskName = "c"
	b := p
	b.AgentID = "a"
	b.TaskName = "b-c"
	first, _ := NewExecutionRecord(a)
	second, _ := NewExecutionRecord(b)
	if first.ID() == second.ID() {
		t.Fatal("separator collision")
	}
	if e.StartedAtMs() != 2000 || e.FinishedAtMs() != 1000 || e.SizeBytes() <= 256 {
		t.Fatal("lost source clock or logical charge")
	}
	for _, status := range []string{"succeeded", "failed", "timeout", "canceled", "exhausted"} {
		next := p
		next.Status = status
		if _, err := NewExecutionRecord(next); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []func(*ExecutionRecordParams){func(x *ExecutionRecordParams) { x.Generation = 0 }, func(x *ExecutionRecordParams) { x.Attempt = 0 }, func(x *ExecutionRecordParams) { x.TaskUID = "" }, func(x *ExecutionRecordParams) { x.Status = "running" }, func(x *ExecutionRecordParams) { x.CapturedAt = time.Time{} }, func(x *ExecutionRecordParams) { x.AgentName = string([]byte{255}) }, func(x *ExecutionRecordParams) { x.TaskName = strings.Repeat("x", 513) }, func(x *ExecutionRecordParams) { x.Error = strings.Repeat("é", 16385) }} {
		next := p
		change(&next)
		if _, err := NewExecutionRecord(next); err == nil {
			t.Fatal("invalid record accepted")
		}
	}
	if !reflect.DeepEqual(e.Params(), e.Clone().Params()) {
		t.Fatal("clone changed fields")
	}
}
