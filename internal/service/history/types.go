package history

import (
	"github.com/soltiHQ/control-plane/internal/storage"
)

type ListQuery struct {
	Limit    int
	Cursor   string
	Criteria storage.ExecutionQueryCriteria
}

// CaptureRequest associates observed runtime identity with an optional managed
// spec. Generation on the resulting record remains the SDK incarnation's one.
type CaptureRequest struct {
	AgentID, AgentName, TaskName, SpecID, SpecName string
}
type CaptureOptions struct {
	PageSize      uint32
	MaxPages      int
	MaxBytes      int64
	Continuation  *RunContinuation
	KnownComplete *SnapshotIdentity
}

// RunContinuation resumes a single UID/version snapshot. A byte bound may stop
// inside a page; AfterGeneration/AfterAttempt then pin the last consumed item
// while Cursor still points at that page. Tokens are collection-local only.
// SnapshotIdentity only identifies a fully captured source snapshot. It is an
// optional process-local optimization; pruning is not permanent deduplication.
type SnapshotIdentity struct{ TaskUID, ResourceVersion string }

type RunContinuation struct {
	Cursor, TaskUID, ResourceVersion string
	AfterGeneration                  uint64
	AfterAttempt                     uint32
}

func DefaultCaptureOptions() CaptureOptions {
	return CaptureOptions{PageSize: 256, MaxPages: 4, MaxBytes: 8 * 1024 * 1024}
}

func (o CaptureOptions) withDefaults() CaptureOptions {
	d := DefaultCaptureOptions()
	if o.PageSize == 0 {
		o.PageSize = d.PageSize
	}
	if o.MaxPages == 0 {
		o.MaxPages = d.MaxPages
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = d.MaxBytes
	}
	return o
}

type CaptureResult struct {
	Observed, Added, Existing, Pruned int
	Partial                           bool
	SourceResourceVersion             string
	TaskUID                           string
	Continuation                      *RunContinuation
	Unchanged                         bool
}
