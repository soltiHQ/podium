package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/soltiHQ/control-plane/domain"
)

const (
	ExecutionMetadataMaxBytes = 512
	ExecutionErrorMaxBytes    = 32 * 1024
)

// ExecutionRecordParams contains a bounded observation of a terminal run.
// CapturedAt is trusted Podium time; source wall-clock run timestamps are kept
// verbatim, including missing timestamps and a clock moving backwards.
type ExecutionRecordParams struct {
	AgentID, AgentName, TaskName, TaskUID string
	Generation                            uint64
	Attempt                               uint32
	Status                                string
	StartedAtMs, FinishedAtMs             int64
	ExitCode                              *int32
	Error                                 string
	WorkloadAPIVersion, WorkloadKind      string
	SpecID, SpecName                      string
	SourceResourceVersion                 string
	CapturedAt                            time.Time
}

// ExecutionRecord is independent of live agent/spec lifecycle. It contains no
// command, environment, credentials or logs. Its run identity is immutable.
type ExecutionRecord struct {
	id     string
	params ExecutionRecordParams
}

var _ domain.Entity[*ExecutionRecord] = (*ExecutionRecord)(nil)

func NewExecutionRecord(p ExecutionRecordParams) (*ExecutionRecord, error) {
	if p.AgentID == "" || p.TaskName == "" || p.TaskUID == "" {
		return nil, fmt.Errorf("execution record: %w", domain.ErrEmptyID)
	}
	if p.Generation == 0 || p.Attempt == 0 {
		return nil, fmt.Errorf("execution record: generation and attempt must be positive")
	}
	if !IsTerminalExecutionStatus(p.Status) {
		return nil, fmt.Errorf("execution record: invalid terminal status %q", p.Status)
	}
	if p.CapturedAt.IsZero() || !time.Unix(0, p.CapturedAt.UnixNano()).Equal(p.CapturedAt) {
		return nil, fmt.Errorf("execution record: invalid capture time")
	}
	for name, value := range map[string]string{"agent ID": p.AgentID, "agent name": p.AgentName, "task name": p.TaskName, "task UID": p.TaskUID, "workload API version": p.WorkloadAPIVersion, "workload kind": p.WorkloadKind, "spec ID": p.SpecID, "spec name": p.SpecName, "resource version": p.SourceResourceVersion} {
		if len(value) > ExecutionMetadataMaxBytes || !utf8.ValidString(value) {
			return nil, fmt.Errorf("execution record: invalid or oversized %s", name)
		}
	}
	if len(p.Error) > ExecutionErrorMaxBytes || !utf8.ValidString(p.Error) {
		return nil, fmt.Errorf("execution record: invalid or oversized error")
	}
	p.CapturedAt = p.CapturedAt.UTC()
	if p.ExitCode != nil {
		value := *p.ExitCode
		p.ExitCode = &value
	}
	digest := sha256.New()
	var size [8]byte
	for _, s := range []string{p.AgentID, p.TaskName, p.TaskUID} {
		binary.BigEndian.PutUint64(size[:], uint64(len(s)))
		digest.Write(size[:])
		digest.Write([]byte(s))
	}
	binary.BigEndian.PutUint64(size[:], p.Generation)
	digest.Write(size[:])
	var attempt [4]byte
	binary.BigEndian.PutUint32(attempt[:], p.Attempt)
	digest.Write(attempt[:])
	return &ExecutionRecord{id: "exec-" + hex.EncodeToString(digest.Sum(nil)), params: p}, nil
}

func IsTerminalExecutionStatus(s string) bool {
	switch s {
	case "succeeded", "failed", "timeout", "canceled", "exhausted":
		return true
	}
	return false
}
func (e *ExecutionRecord) ID() string                    { return e.id }
func (e *ExecutionRecord) CreatedAt() time.Time          { return e.params.CapturedAt }
func (e *ExecutionRecord) UpdatedAt() time.Time          { return e.params.CapturedAt }
func (e *ExecutionRecord) CapturedAt() time.Time         { return e.params.CapturedAt }
func (e *ExecutionRecord) AgentID() string               { return e.params.AgentID }
func (e *ExecutionRecord) AgentName() string             { return e.params.AgentName }
func (e *ExecutionRecord) TaskName() string              { return e.params.TaskName }
func (e *ExecutionRecord) TaskUID() string               { return e.params.TaskUID }
func (e *ExecutionRecord) Generation() uint64            { return e.params.Generation }
func (e *ExecutionRecord) Attempt() uint32               { return e.params.Attempt }
func (e *ExecutionRecord) Status() string                { return e.params.Status }
func (e *ExecutionRecord) StartedAtMs() int64            { return e.params.StartedAtMs }
func (e *ExecutionRecord) FinishedAtMs() int64           { return e.params.FinishedAtMs }
func (e *ExecutionRecord) Error() string                 { return e.params.Error }
func (e *ExecutionRecord) WorkloadAPIVersion() string    { return e.params.WorkloadAPIVersion }
func (e *ExecutionRecord) WorkloadKind() string          { return e.params.WorkloadKind }
func (e *ExecutionRecord) SpecID() string                { return e.params.SpecID }
func (e *ExecutionRecord) SpecName() string              { return e.params.SpecName }
func (e *ExecutionRecord) SourceResourceVersion() string { return e.params.SourceResourceVersion }
func (e *ExecutionRecord) ExitCode() *int32 {
	if e.params.ExitCode == nil {
		return nil
	}
	v := *e.params.ExitCode
	return &v
}

func (e *ExecutionRecord) Params() ExecutionRecordParams {
	p := e.params
	p.ExitCode = e.ExitCode()
	return p
}

func (e *ExecutionRecord) Clone() *ExecutionRecord {
	if e == nil {
		return nil
	}
	return &ExecutionRecord{id: e.id, params: e.Params()}
}

// SizeBytes charges a conservative deterministic logical payload budget. It
// is not an estimate of database allocation, resident memory, or wire size.
func (e *ExecutionRecord) SizeBytes() int64 {
	total := int64(256)
	for _, s := range []string{e.id, e.params.AgentID, e.params.AgentName, e.params.TaskName, e.params.TaskUID, e.params.Status, e.params.Error, e.params.WorkloadAPIVersion, e.params.WorkloadKind, e.params.SpecID, e.params.SpecName, e.params.SourceResourceVersion} {
		total += int64(len(s))
	}
	return total
}
