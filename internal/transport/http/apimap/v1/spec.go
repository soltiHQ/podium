package apimapv1

import (
	"encoding/json"
	"time"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/proxy"
)

// Spec maps a domain Spec to its REST DTO.
func Spec(ts *model.Spec) restv1.Spec {
	if ts == nil {
		return restv1.Spec{}
	}

	out := restv1.Spec{
		ID:                ts.ID(),
		Name:              ts.Name(),
		Slot:              ts.Slot(),
		Version:           ts.Version(),
		Generation:        ts.Generation(),
		DeletionRequested: ts.DeletionRequested(),

		KindType:   string(ts.KindType()),
		KindConfig: ts.KindConfig(),

		TimeoutMs:      ts.TimeoutMs(),
		IntervalMs:     ts.IntervalMs(),
		BackoffFirstMs: ts.Backoff().FirstMs,
		BackoffMaxMs:   ts.Backoff().MaxMs,
		BackoffFactor:  ts.Backoff().Factor,

		Jitter:      string(ts.Backoff().Jitter),
		RestartType: string(ts.RestartType()),

		Targets:      ts.Targets(),
		TargetLabels: ts.TargetLabels(),
		RunnerLabels: ts.RunnerLabels(),

		CreatedAt: ts.CreatedAt().Format(time.RFC3339),
		UpdatedAt: ts.UpdatedAt().Format(time.RFC3339),
	}

	if proto, err := proxy.SpecToProto(ts); err == nil {
		if preview, perr := proxy.CreateSpecWirePreview(proto); perr == nil && len(preview) > 0 {
			out.CreateSpec = json.RawMessage(preview)
		}
	}

	return out
}
