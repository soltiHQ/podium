package spec

import (
	"context"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/workloadschema"
)

// testSchema is the workload schema the service tests validate against.
const testSchema = `{"type":"object","required":["image"],"properties":{"image":{"type":"string"},"replicas":{"type":"integer"}},"additionalProperties":false}`

const (
	testAPIVersion = "solti.io/v1"
	testKind       = "Container"
)

// testFingerprint is the fingerprint of testSchema.
func testFingerprint(t testing.TB) string {
	t.Helper()
	fp, err := workloadschema.Fingerprint([]byte(testSchema))
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// seedTestSchema puts testSchema into the catalog of the store.
func seedTestSchema(t testing.TB, store storage.Storage) {
	t.Helper()
	entry, err := model.NewWorkloadSchema(testFingerprint(t), testAPIVersion, testKind, []byte(testSchema), "agent-1", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorkloadSchema(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
}

// testWorkload is a workload that testSchema accepts.
func testWorkload(t testing.TB) model.Workload {
	t.Helper()
	return model.Workload{
		APIVersion:        testAPIVersion,
		Kind:              testKind,
		SchemaFingerprint: testFingerprint(t),
		Body:              map[string]any{"image": "busybox"},
	}
}
