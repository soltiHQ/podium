package dto

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
)

type BackoffDTO struct {
	Jitter  string // kind.JitterStrategy
	FirstMs int64
	MaxMs   int64
	Factor  float64
}

type SpecDTO struct {
	ID                string
	Name              string
	Slot              string
	Version           int
	Generation        int
	DeletionRequested bool

	KindType    string // kind.TaskKindType
	KindConfig  map[string]any
	TimeoutMs   int64
	RestartType string // kind.RestartType
	IntervalMs  int64
	Backoff     BackoffDTO

	Targets      []string
	TargetLabels map[string]string
	RunnerLabels map[string]string

	CreatedAt time.Time
	UpdatedAt time.Time
}

func SpecToDTO(ts *model.Spec) *SpecDTO {
	b := ts.Backoff()
	tl := make(map[string]string, len(ts.TargetLabels()))
	for k, v := range ts.TargetLabels() {
		tl[k] = v
	}
	rl := make(map[string]string, len(ts.RunnerLabels()))
	for k, v := range ts.RunnerLabels() {
		rl[k] = v
	}
	kc := make(map[string]any, len(ts.KindConfig()))
	for k, v := range ts.KindConfig() {
		kc[k] = v
	}
	tg := make([]string, len(ts.Targets()))
	copy(tg, ts.Targets())

	return &SpecDTO{
		ID:                ts.ID(),
		Name:              ts.Name(),
		Slot:              ts.Slot(),
		Version:           ts.Version(),
		Generation:        ts.Generation(),
		DeletionRequested: ts.DeletionRequested(),
		KindType:          string(ts.KindType()),
		KindConfig:        kc,
		TimeoutMs:         ts.TimeoutMs(),
		RestartType:       string(ts.RestartType()),
		IntervalMs:        ts.IntervalMs(),
		Backoff: BackoffDTO{
			Jitter:  string(b.Jitter),
			FirstMs: b.FirstMs,
			MaxMs:   b.MaxMs,
			Factor:  b.Factor,
		},
		Targets:      tg,
		TargetLabels: tl,
		RunnerLabels: rl,
		CreatedAt:    ts.CreatedAt(),
		UpdatedAt:    ts.UpdatedAt(),
	}
}

func SpecFromDTO(d *SpecDTO) (*model.Spec, error) {
	if d == nil {
		return nil, nil
	}
	ts, err := model.NewSpec(d.ID, d.Name, d.Slot)
	if err != nil {
		return nil, err
	}
	ts.SetKindType(kindTaskKindType(d.KindType))
	ts.SetKindConfig(d.KindConfig)
	ts.SetTimeoutMs(d.TimeoutMs)
	ts.SetRestartType(kindRestartType(d.RestartType))
	ts.SetIntervalMs(d.IntervalMs)
	ts.SetBackoff(model.BackoffConfig{
		Jitter:  kind.JitterStrategy(d.Backoff.Jitter),
		FirstMs: d.Backoff.FirstMs,
		MaxMs:   d.Backoff.MaxMs,
		Factor:  d.Backoff.Factor,
	})
	ts.SetTargets(d.Targets)
	ts.SetTargetLabels(d.TargetLabels)
	ts.SetRunnerLabels(d.RunnerLabels)
	ts.SetVersion(d.Version)
	ts.SetGeneration(d.Generation)
	ts.SetDeletionRequested(d.DeletionRequested)
	ts.SetCreatedAt(d.CreatedAt)
	ts.SetUpdatedAt(d.UpdatedAt)
	return ts, nil
}
