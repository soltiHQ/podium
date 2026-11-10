package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// inventoryTimeout bounds what a scrape waits for the store.
const inventoryTimeout = 5 * time.Second

var (
	agentStatuses   = []enum.AgentStatus{enum.AgentStatusActive, enum.AgentStatusInactive, enum.AgentStatusDisconnected}
	rolloutStatuses = []enum.SyncStatus{enum.SyncStatusPending, enum.SyncStatusSynced, enum.SyncStatusDrift, enum.SyncStatusFailed, enum.SyncStatusUnknown}

	agentsDesc = prometheus.NewDesc("podium_agents",
		"Agents in this node's store by status, read at scrape; a raft follower reads its local replica.", []string{"status"}, nil)
	rolloutsDesc = prometheus.NewDesc("podium_rollouts",
		"Rollouts in this node's store by sync status, read at scrape; a raft follower reads its local replica.", []string{"status"}, nil)
)

// Inventory registers podium_agents and podium_rollouts over store. Each scrape
// counts the rows of each kind with one read of the store (storage.CountAgents,
// which counts in place when the store can and copies no row). A kind whose count
// fails has no series in that scrape.
func (m *Metrics) Inventory(store storage.Storage) {
	if m == nil {
		return
	}
	m.reg.MustRegister(inventory{store: store})
}

// inventory is the collector of the inventory gauges.
type inventory struct{ store storage.Storage }

func (inventory) Describe(ch chan<- *prometheus.Desc) {
	ch <- agentsDesc
	ch <- rolloutsDesc
}

func (i inventory) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), inventoryTimeout)
	defer cancel()
	if counts, err := storage.CountAgents(ctx, i.store); err == nil {
		for _, s := range agentStatuses {
			ch <- prometheus.MustNewConstMetric(agentsDesc, prometheus.GaugeValue, float64(counts[s]), s.String())
		}
	}
	if counts, err := storage.CountRollouts(ctx, i.store); err == nil {
		for _, s := range rolloutStatuses {
			ch <- prometheus.MustNewConstMetric(rolloutsDesc, prometheus.GaugeValue, float64(counts[s]), s.String())
		}
	}
}
