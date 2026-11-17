package model

import (
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/soltiHQ/control-plane/domain"
)

// workloadSchemaFingerprint is the form of a fingerprint: the lowercase hex of
// a sha256 digest.
var workloadSchemaFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

// WorkloadSchema is one schema of the catalog of workload schemas, keyed by its
// fingerprint: the schema bytes an agent offered for a workload type, the type
// it was first offered for, the agent that offered it and when. The schema is
// untrusted text; the catalog keeps it as received and never interprets it. An
// entry never changes: the schema under a fingerprint is the same bytes.
type WorkloadSchema struct {
	fingerprint  string
	apiVersion   string
	kind         string
	schema       []byte
	firstAgentID string
	firstSeenAt  time.Time
}

var _ domain.Entity[*WorkloadSchema] = (*WorkloadSchema)(nil)

// NewWorkloadSchema returns the catalog entry of the schema under the
// fingerprint. The fingerprint must be 64 lowercase hex characters; every
// other field must be set, the strings valid UTF-8, and the time one a message
// of Unix nanoseconds carries. The schema is copied.
func NewWorkloadSchema(fingerprint, apiVersion, kind string, schema []byte, firstAgentID string, firstSeenAt time.Time) (*WorkloadSchema, error) {
	if !workloadSchemaFingerprint.MatchString(fingerprint) {
		return nil, fmt.Errorf("workload schema: the fingerprint is not 64 lowercase hex characters")
	}
	for name, value := range map[string]string{"API version": apiVersion, "kind": kind, "first agent ID": firstAgentID} {
		if value == "" {
			return nil, fmt.Errorf("workload schema: the %s is empty", name)
		}
		if !utf8.ValidString(value) {
			return nil, fmt.Errorf("workload schema: the %s is not valid UTF-8", name)
		}
	}
	if len(schema) == 0 {
		return nil, fmt.Errorf("workload schema: the schema is empty")
	}
	// A message of Unix nanoseconds reads 0 back as the zero time, so the Unix
	// epoch itself is no first-seen time either.
	if firstSeenAt.IsZero() || firstSeenAt.UnixNano() == 0 || !time.Unix(0, firstSeenAt.UnixNano()).Equal(firstSeenAt) {
		return nil, fmt.Errorf("workload schema: invalid first-seen time")
	}
	return &WorkloadSchema{
		fingerprint:  fingerprint,
		apiVersion:   apiVersion,
		kind:         kind,
		schema:       append([]byte(nil), schema...),
		firstAgentID: firstAgentID,
		firstSeenAt:  firstSeenAt.UTC(),
	}, nil
}

// ID returns the fingerprint.
func (w *WorkloadSchema) ID() string { return w.fingerprint }

// CreatedAt returns when the schema was first seen.
func (w *WorkloadSchema) CreatedAt() time.Time { return w.firstSeenAt }

// UpdatedAt returns when the schema was first seen: an entry never changes.
func (w *WorkloadSchema) UpdatedAt() time.Time { return w.firstSeenAt }

// APIVersion returns the API version of the workload type the schema was first offered for.
func (w *WorkloadSchema) APIVersion() string { return w.apiVersion }

// Kind returns the kind of the workload type the schema was first offered for.
func (w *WorkloadSchema) Kind() string { return w.kind }

// Schema returns a copy of the schema bytes.
func (w *WorkloadSchema) Schema() []byte { return append([]byte(nil), w.schema...) }

// FirstAgentID returns the ID of the agent that first offered the schema.
func (w *WorkloadSchema) FirstAgentID() string { return w.firstAgentID }

// FirstSeenAt returns when the schema was first offered.
func (w *WorkloadSchema) FirstSeenAt() time.Time { return w.firstSeenAt }

// Clone returns a deep copy.
func (w *WorkloadSchema) Clone() *WorkloadSchema {
	if w == nil {
		return nil
	}
	c := *w
	c.schema = append([]byte(nil), w.schema...)
	return &c
}
