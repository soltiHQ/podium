package metrics

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
)

// The values of the pool label of the podium_database_pool_* metrics: the pools
// of the database mode.
const (
	// PoolPrimary is the primary's pool, which every write and every decision
	// reads.
	PoolPrimary = "primary"
	// PoolReplica is the read replica's pool, of cluster.database.replica_url.
	PoolReplica = "replica"
	// PoolLease is the lease's pool of one connection.
	PoolLease = "lease"
)

var (
	poolMaxOpenDesc = prometheus.NewDesc("podium_database_pool_max_open_connections",
		"The cap of the pool's connections, by pool (primary, replica or lease).", []string{"pool"}, nil)
	poolOpenDesc = prometheus.NewDesc("podium_database_pool_open_connections",
		"The pool's connections, in use and idle, by pool.", []string{"pool"}, nil)
	poolInUseDesc = prometheus.NewDesc("podium_database_pool_in_use_connections",
		"The pool's connections in use, by pool.", []string{"pool"}, nil)
	poolIdleDesc = prometheus.NewDesc("podium_database_pool_idle_connections",
		"The pool's idle connections, by pool.", []string{"pool"}, nil)
	poolWaitCountDesc = prometheus.NewDesc("podium_database_pool_wait_count_total",
		"Connections the pool's callers waited for, the pool at its cap, by pool.", []string{"pool"}, nil)
	poolWaitDurationDesc = prometheus.NewDesc("podium_database_pool_wait_duration_seconds_total",
		"Seconds the pool's callers waited for a connection, by pool.", []string{"pool"}, nil)
)

// Database registers the metrics of the database mode: the statistics of the
// primary's pool, the replica's, which is nil without cluster.database.replica_url
// and then has no series, and the lease's, under their pool label;
// podium_database_leader, from leads; podium_database_events_dropped_total, from
// dropped; all read at scrape; and the lease's counters, which LeaseRenewed and
// LeaseLost count. No label carries a URL, a host or a query parameter: the pool
// label is one of PoolPrimary, PoolReplica and PoolLease.
func (m *Metrics) Database(primary, replica, lease *sql.DB, leads func() bool, dropped func() uint64) {
	if m == nil {
		return
	}
	var dbs pools
	for _, p := range []struct {
		name string
		db   *sql.DB
	}{{PoolPrimary, primary}, {PoolReplica, replica}, {PoolLease, lease}} {
		if p.db != nil {
			dbs = append(dbs, pool{name: p.name, db: p.db})
		}
	}
	m.reg.MustRegister(
		dbs,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "podium_database_leader",
			Help: "1 while this replica holds the database lease, the leader, otherwise 0.",
		}, func() float64 {
			if leads() {
				return 1
			}
			return 0
		}),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "podium_database_events_dropped_total",
			Help: "Events that never reached the database: each was applied on this replica alone.",
		}, func() float64 { return float64(dropped()) }),
		m.renewals, m.losses,
	)
}

// LeaseRenewed counts a renewal of the database lease that landed.
func (m *Metrics) LeaseRenewed() {
	if m == nil {
		return
	}
	m.renewals.Inc()
}

// LeaseLost counts a term of this replica that ended before its stop.
func (m *Metrics) LeaseLost() {
	if m == nil {
		return
	}
	m.losses.Inc()
}

// pool is a pool of the database mode and the value of its pool label.
type pool struct {
	name string
	db   *sql.DB
}

// pools is the collector of the pools' statistics, which it reads at each
// scrape.
type pools []pool

func (pools) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{poolMaxOpenDesc, poolOpenDesc, poolInUseDesc, poolIdleDesc, poolWaitCountDesc, poolWaitDurationDesc} {
		ch <- d
	}
}

func (ps pools) Collect(ch chan<- prometheus.Metric) {
	for _, p := range ps {
		s := p.db.Stats()
		ch <- prometheus.MustNewConstMetric(poolMaxOpenDesc, prometheus.GaugeValue, float64(s.MaxOpenConnections), p.name)
		ch <- prometheus.MustNewConstMetric(poolOpenDesc, prometheus.GaugeValue, float64(s.OpenConnections), p.name)
		ch <- prometheus.MustNewConstMetric(poolInUseDesc, prometheus.GaugeValue, float64(s.InUse), p.name)
		ch <- prometheus.MustNewConstMetric(poolIdleDesc, prometheus.GaugeValue, float64(s.Idle), p.name)
		ch <- prometheus.MustNewConstMetric(poolWaitCountDesc, prometheus.CounterValue, float64(s.WaitCount), p.name)
		ch <- prometheus.MustNewConstMetric(poolWaitDurationDesc, prometheus.CounterValue, s.WaitDuration.Seconds(), p.name)
	}
}
