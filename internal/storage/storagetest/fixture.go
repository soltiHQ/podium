// Package storagetest builds the state that the storage and service benchmarks
// run against. It writes through storage.Storage alone, so every backend gets
// the same rows and a before/after comparison stays like for like.
package storagetest

import (
	"context"
	"fmt"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Epoch stamps everything the fixture builds. No timestamp in it comes from the
// wall clock, so equal fixtures hold equal rows. A benchmark that needs
// liveness or retention decided pins its own clock relative to Epoch.
var Epoch = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

const (
	// SpecCount is the number of specs in every fixture.
	SpecCount = 20

	// RolloutsPerAgent is how many specs each agent has a rollout of.
	RolloutsPerAgent = 3

	// ExplicitTargets is the number of agents ExplicitSpec lists by ID.
	ExplicitTargets = 20

	// HeartbeatInterval is the interval every agent reports.
	HeartbeatInterval = 30 * time.Second

	// labelSpecs is how many specs, from the first, select agents by label. The
	// rest list agents by ID.
	labelSpecs = 10
)

var (
	// FleetSpec selects every agent: all of them carry the label env=prod.
	FleetSpec = SpecID(0)

	// ExplicitSpec lists the first ExplicitTargets agents by ID.
	ExplicitSpec = SpecID(labelSpecs)

	regions = [...]string{"eu-west", "eu-east", "us-west", "us-east"}
	teams   = [...]string{"ops", "data", "edge", "infra", "apps"}
	zones   = [...]string{"a", "b", "c"}
)

// Fixture is the size of the state Seed builds: SpecCount specs, Agents agents
// and HistoryRows terminal execution records.
//
// Per agent it builds one row with 6 metadata entries, 3 labels and 2 runners,
// one credential and RolloutsPerAgent rollouts, about 900 B in a snapshot.
type Fixture struct {
	Agents      int
	HistoryRows int
}

// AgentID is the ID of the i-th agent.
func AgentID(i int) string { return fmt.Sprintf("agent-%05d", i) }

// AgentToken is the bearer token enrolled for the i-th agent.
func AgentToken(i int) string { return fmt.Sprintf("tok-%060d", i) }

// SpecID is the ID of the j-th spec.
func SpecID(j int) string { return fmt.Sprintf("spec-%02d", j) }

// slotName is the task slot of the j-th spec, and the ID an agent gives its task.
func slotName(j int) string { return fmt.Sprintf("slot-%02d", j) }

// AgentParams is the discovery payload of the i-th agent. Seed builds the agent
// from it and a heartbeat benchmark sends it again, as the agent would.
func AgentParams(i int) model.AgentParams {
	return model.AgentParams{
		ID:       AgentID(i),
		Name:     fmt.Sprintf("edge-%05d", i),
		Endpoint: fmt.Sprintf("http://10.%d.%d.%d:8085", (i>>16)&0xff, (i>>8)&0xff, i&0xff),

		UptimeSeconds:      3600,
		HeartbeatIntervalS: int(HeartbeatInterval / time.Second),

		EndpointType: 2,
		APIVersion:   1,

		OS:       "linux",
		Arch:     [...]string{"amd64", "arm64"}[i%2],
		Platform: "ubuntu-24.04",

		Metadata: map[string]string{
			"hostname": fmt.Sprintf("edge-%05d.dc1.example.net", i),
			"kernel":   "6.8.0-45-generic",
			"cpu":      "Xeon Gold 6338",
			"mem_mb":   "16384",
			"sdk":      "0.4.2",
			"boot_id":  fmt.Sprintf("%016x", i),
		},
		Capabilities: []model.RunnerCapability{
			{
				Name:      "subprocess",
				Labels:    map[string]string{"pool": "general", "zone": zones[i%len(zones)]},
				Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Subprocess"}},
			},
			{
				Name:      "container",
				Labels:    map[string]string{"pool": "general", "zone": zones[i%len(zones)]},
				Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Container"}},
			},
		},
	}
}

// Seed writes the fixture into s in one transaction, so it is all or nothing.
// It expects an empty store; seeding the same fixture twice leaves the same rows.
func Seed(ctx context.Context, s storage.Storage, f Fixture) error {
	if f.Agents < 0 || f.HistoryRows < 0 {
		return fmt.Errorf("%w: fixture sizes must not be negative", storage.ErrInvalidArgument)
	}
	return s.WithTx(ctx, func(tx storage.Storage) error {
		for j := range SpecCount {
			ts, err := newSpec(j)
			if err != nil {
				return err
			}
			if err = tx.UpsertSpec(ctx, ts); err != nil {
				return err
			}
		}
		for i := range f.Agents {
			if err := seedAgent(ctx, tx, i); err != nil {
				return err
			}
		}
		for i := range f.HistoryRows {
			rec, err := newExecution(i)
			if err != nil {
				return err
			}
			if err = tx.UpsertExecution(ctx, rec); err != nil {
				return err
			}
		}
		return nil
	})
}

func newSpec(j int) (*model.Spec, error) {
	ts, err := model.NewSpec(SpecID(j), fmt.Sprintf("job-%02d", j), slotName(j))
	if err != nil {
		return nil, err
	}
	ts.SetKindConfig(map[string]any{"command": map[string]any{"command": "/usr/local/bin/job", "args": []any{"--shard", fmt.Sprint(j)}}})
	if j < labelSpecs {
		selector := map[string]string{"env": "prod"}
		if j > 0 {
			selector["region"] = regions[j%len(regions)]
		}
		ts.SetTargetLabels(selector)
	} else {
		targets := make([]string, ExplicitTargets)
		for i := range targets {
			targets[i] = AgentID(i)
		}
		ts.SetTargets(targets)
	}
	// The setters above stamp the wall clock; the fixture must not carry it.
	ts.SetCreatedAt(Epoch)
	ts.SetUpdatedAt(Epoch)
	return ts, nil
}

// seedAgent writes the i-th agent, its credential and its rollouts, which are
// of the specs i, i+1 and i+2 (modulo SpecCount). Agents are created one minute
// apart, newest first, so a listing returns them in index order.
func seedAgent(ctx context.Context, tx storage.Storage, i int) error {
	a, err := model.NewAgentFrom(AgentParams(i))
	if err != nil {
		return err
	}
	a.SetLabels(map[string]string{"env": "prod", "region": regions[i%len(regions)], "team": teams[i%len(teams)]})
	a.SetCreatedAt(Epoch.Add(-time.Duration(i) * time.Minute))
	a.SetUpdatedAt(Epoch)
	a.SetLastSeenAt(Epoch)
	a.SetStaleAt(Epoch.Add(HeartbeatInterval))
	if err = tx.UpsertAgent(ctx, a); err != nil {
		return err
	}

	cred, err := model.NewAgentCredential(a.ID(), AgentToken(i))
	if err != nil {
		return err
	}
	cred.SetCreatedAt(Epoch)
	cred.SetUpdatedAt(Epoch)
	if err = tx.UpsertAgentCredential(ctx, cred); err != nil {
		return err
	}

	for k := range RolloutsPerAgent {
		j := (i + k) % SpecCount
		ro, err := model.NewRollout(SpecID(j), a.ID(), 1)
		if err != nil {
			return err
		}
		ro.RestoreState(enum.SyncStatusSynced, enum.RolloutIntentNoop, 1, 0, "", Epoch, Epoch)
		ro.SetActualTaskID(slotName(j))
		ro.SetCreatedAt(Epoch)
		ro.SetUpdatedAt(Epoch)
		if err = tx.UpsertRollout(ctx, ro); err != nil {
			return err
		}
	}
	return nil
}

// newExecution builds the i-th terminal record. Records are captured a second
// apart, oldest first, and carry no live agent or spec.
func newExecution(i int) (*model.ExecutionRecord, error) {
	status, errText, exit := "succeeded", "", int32(0)
	if i%7 == 0 {
		status, errText, exit = "failed", "exit status 1", 1
	}
	started := Epoch.UnixMilli() + int64(i)*1000
	name := fmt.Sprintf("job-%02d", i%SpecCount)
	return model.NewExecutionRecord(model.ExecutionRecordParams{
		AgentID:   AgentID(i % 500),
		AgentName: fmt.Sprintf("edge-%05d", i%500),
		TaskName:  name,
		TaskUID:   fmt.Sprintf("uid-%08d", i),

		Generation:   1,
		Attempt:      uint32(i%3) + 1,
		Status:       status,
		StartedAtMs:  started,
		FinishedAtMs: started + 800,
		ExitCode:     &exit,
		Error:        errText,

		WorkloadAPIVersion:    "solti.io/v1",
		WorkloadKind:          "Subprocess",
		SpecID:                SpecID(i % SpecCount),
		SpecName:              name,
		SourceResourceVersion: fmt.Sprintf("rv-%d", i),

		CapturedAt: Epoch.Add(time.Duration(i) * time.Second),
	})
}
