package model

import (
	"encoding/json"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
)

// NewSpec starts at generation=1, matching version=1. Both counters are
// 1 out of the gate — only Upsert drifts them apart (metadata edits bump
// only version; runtime edits bump both).
func TestNewSpecDefaultsVersionAndGeneration(t *testing.T) {
	ts, err := NewSpec("sp-1", "demo", "slot-1")
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	if got, want := ts.Version(), 1; got != want {
		t.Errorf("version: got %d, want %d", got, want)
	}
	if got, want := ts.Generation(), 1; got != want {
		t.Errorf("generation: got %d, want %d", got, want)
	}
	if ts.DeletionRequested() {
		t.Errorf("DeletionRequested should be false on fresh spec")
	}
}

// BumpGeneration and IncrementVersion move independently. This is the
// core invariant behind the two-counter design: service-layer Upsert
// always bumps version, and only bumps generation when RuntimeEquals
// reports false.
func TestVersionAndGenerationAreIndependent(t *testing.T) {
	ts, _ := NewSpec("sp-1", "demo", "slot-1")
	ts.IncrementVersion()
	ts.IncrementVersion()
	ts.BumpGeneration()
	if got, want := ts.Version(), 3; got != want {
		t.Errorf("version: got %d, want %d", got, want)
	}
	if got, want := ts.Generation(), 2; got != want {
		t.Errorf("generation: got %d, want %d", got, want)
	}
}

// MarkForDeletion flips the soft-delete flag. The service keeps the
// record alive until the sync runner finalizer drops it.
func TestMarkForDeletionFlipsFlag(t *testing.T) {
	ts, _ := NewSpec("sp-1", "demo", "slot-1")
	if ts.DeletionRequested() {
		t.Fatal("precondition: DeletionRequested should be false")
	}
	ts.MarkForDeletion()
	if !ts.DeletionRequested() {
		t.Fatalf("MarkForDeletion should set DeletionRequested=true")
	}
}

// Clone carries every new field over. Service layer clones on Get/List,
// so a leak here would silently desynchronise the live object from its
// stored copy.
func TestCloneCopiesNewFields(t *testing.T) {
	ts, _ := NewSpec("sp-1", "demo", "slot-1")
	ts.BumpGeneration()
	ts.MarkForDeletion()

	cp := ts.Clone()

	// Mutate original — clone must not observe these changes.
	ts.IncrementVersion()
	ts.BumpGeneration()

	if cp.Generation() != 2 {
		t.Errorf("clone.Generation leaked: got %d, want 2", cp.Generation())
	}
	if !cp.DeletionRequested() {
		t.Errorf("clone.DeletionRequested should be true")
	}
	if cp.Version() != 1 {
		t.Errorf("clone.Version leaked: got %d, want 1", cp.Version())
	}
}

// RuntimeEquals is what the service uses to decide whether an Upsert
// warrants a generation bump. It compares runtime fields only — name,
// targets, timestamps, version/generation values themselves are ignored.
func TestRuntimeEqualsIgnoresMetadata(t *testing.T) {
	a, _ := NewSpec("sp-1", "orig", "slot-1")
	b, _ := NewSpec("sp-2", "renamed", "slot-1") // different id+name, identical runtime

	// Targets differ.
	b.SetTargets([]string{"agent-a", "agent-b"})

	if !a.RuntimeEquals(b) {
		t.Error("specs with identical runtime fields but different metadata should compare equal")
	}
}

// Every field backing SpecToProto must flip RuntimeEquals to false.
// Covers slot, kindType, kindConfig, timeoutMs, restartType, intervalMs,
// backoff, runnerLabels.
func TestRuntimeEqualsDetectsEveryRuntimeField(t *testing.T) {
	base := func() *Spec {
		s, _ := NewSpec("sp-1", "demo", "slot-1")
		s.SetKindConfig(map[string]any{"command": "sleep", "args": []any{"1"}})
		s.SetRunnerLabels(map[string]string{"zone": "eu"})
		return s
	}

	cases := []struct {
		name   string
		mutate func(*Spec)
	}{
		{"kindType", func(s *Spec) { s.SetKindType(enum.TaskKindWasm) }},
		{"kindConfig value", func(s *Spec) { s.SetKindConfig(map[string]any{"command": "echo"}) }},
		{"timeoutMs", func(s *Spec) { s.SetTimeoutMs(60_000) }},
		{"restartType", func(s *Spec) { s.SetRestartType(enum.RestartAlways) }},
		{"intervalMs", func(s *Spec) { s.SetIntervalMs(500) }},
		{"backoff", func(s *Spec) {
			s.SetBackoff(BackoffConfig{Jitter: enum.JitterFull, FirstMs: 2000, MaxMs: 10_000, Factor: 3.0})
		}},
		{"runnerLabels", func(s *Spec) { s.SetRunnerLabels(map[string]string{"zone": "us"}) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base()
			b := base()
			if !a.RuntimeEquals(b) {
				t.Fatal("precondition: base specs must be RuntimeEqual")
			}
			tc.mutate(b)
			if a.RuntimeEquals(b) {
				t.Errorf("%s change not detected by RuntimeEquals", tc.name)
			}
		})
	}
}

// slot is immutable (no setter), so it can't be covered by the mutator
// table above — build two specs with different slots directly. RuntimeEquals
// must still treat slot as a runtime field.
func TestRuntimeEqualsDetectsSlot(t *testing.T) {
	a, _ := NewSpec("sp-1", "demo", "slot-1")
	b, _ := NewSpec("sp-1", "demo", "slot-2")
	if a.RuntimeEquals(b) {
		t.Error("slot difference not detected by RuntimeEquals")
	}
}

// Nil handling: both nil → equal; one nil → not equal. Keeps the
// caller-site ergonomics simple ("if !new.RuntimeEquals(old) …") even
// when old is missing on create.
func TestRuntimeEqualsHandlesNil(t *testing.T) {
	a, _ := NewSpec("sp-1", "demo", "slot-1")

	var n *Spec
	if a.RuntimeEquals(n) {
		t.Error("non-nil vs nil should not be equal")
	}
	if n.RuntimeEquals(a) {
		t.Error("nil vs non-nil should not be equal")
	}
	if !n.RuntimeEquals(n) {
		t.Error("nil vs nil should be equal")
	}
}

// TestRuntimeEqualsSeesABodyChange: the workload is part of the runtime
// fields, so a change of its body, type or schema is a runtime change, and a
// copy of it, or the same numbers held another way, is not.
func TestRuntimeEqualsSeesABodyChange(t *testing.T) {
	base := Workload{APIVersion: "solti.io/v1", Kind: "Container", SchemaFingerprint: "f1",
		Body: map[string]any{"image": "busybox", "ports": []any{float64(80), map[string]any{"n": "x"}}}}
	a, _ := NewSpec("sp-1", "n", "slot")
	a.SetWorkload(base)
	b := a.Clone()
	if !a.RuntimeEquals(b) {
		t.Fatal("a clone differs from its original")
	}
	// A clone owns its body.
	b.SetWorkload(base)
	base.Body["ports"].([]any)[1].(map[string]any)["n"] = "changed"
	if !a.RuntimeEquals(b) {
		t.Fatal("changing the caller's body reached the spec")
	}

	for name, mutate := range map[string]func(w *Workload){
		"nested body value": func(w *Workload) { w.Body["ports"].([]any)[1].(map[string]any)["n"] = "y" },
		"added body key":    func(w *Workload) { w.Body["extra"] = true },
		"api version":       func(w *Workload) { w.APIVersion = "solti.io/v2" },
		"kind":              func(w *Workload) { w.Kind = "Other" },
		"fingerprint":       func(w *Workload) { w.SchemaFingerprint = "f2" },
	} {
		c := a.Clone()
		w := c.Workload()
		mutate(&w)
		c.SetWorkload(w)
		if a.RuntimeEquals(c) {
			t.Errorf("%s: not seen by RuntimeEquals", name)
		}
	}

	// Numbers are the same number however held.
	n1, n2 := Workload{Body: map[string]any{"n": float64(3)}}, Workload{Body: map[string]any{"n": json.Number("3")}}
	if !n1.Equal(n2) {
		t.Error("float64 3 and json.Number 3 differ")
	}
	if n1.Equal(Workload{Body: map[string]any{"n": json.Number("4")}}) {
		t.Error("3 equals 4")
	}
	if !(Workload{}).Equal(Workload{Body: map[string]any{}}) {
		t.Error("a nil body differs from an empty one")
	}
}

func TestWorkloadGVK(t *testing.T) {
	gvk, err := Workload{APIVersion: "solti.io/v1", Kind: "Container", SchemaFingerprint: "f"}.GVK()
	if err != nil || gvk != (GroupVersionKind{Group: "solti.io", Version: "v1", Kind: "Container", SchemaFingerprint: "f"}) {
		t.Fatalf("GVK = %+v, %v", gvk, err)
	}
	for _, w := range []Workload{{Kind: "K"}, {APIVersion: "v1", Kind: "K"}, {APIVersion: "g/", Kind: "K"}, {APIVersion: "/v1", Kind: "K"}, {APIVersion: "a/b/c", Kind: "K"}, {APIVersion: "g/v1"}} {
		if _, err := w.GVK(); err == nil {
			t.Errorf("GVK accepted %+v", w)
		}
	}
}
