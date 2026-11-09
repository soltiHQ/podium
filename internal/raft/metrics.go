package raft

import (
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	hraft "github.com/hashicorp/raft"
)

var (
	stateDesc = prometheus.NewDesc("podium_raft_state",
		"1 for the raft state this node is in (follower, candidate, leader or shutdown), 0 for the others.", []string{"state"}, nil)
	termDesc = prometheus.NewDesc("podium_raft_term",
		"The current raft term of this node.", nil, nil)
	lastIndexDesc = prometheus.NewDesc("podium_raft_last_index",
		"The index of the last entry in this node's raft log.", nil, nil)
	appliedIndexDesc = prometheus.NewDesc("podium_raft_applied_index",
		"The index of the last log entry this node applied to its state.", nil, nil)
	lastContactDesc = prometheus.NewDesc("podium_raft_last_contact_seconds",
		"Seconds since this follower last heard from the leader; absent on a node that is not a follower, and before the first contact.", nil, nil)
	peersDesc = prometheus.NewDesc("podium_raft_peers",
		"The voting members of the raft configuration besides this node.", nil, nil)

	raftStates = []hraft.RaftState{hraft.Follower, hraft.Candidate, hraft.Leader, hraft.Shutdown}
)

// Collector returns the collector of this node's raft metrics, which a scrape
// reads from the raft node: podium_raft_state, podium_raft_term,
// podium_raft_last_index, podium_raft_applied_index, podium_raft_peers, and on a
// follower podium_raft_last_contact_seconds. None has a label that names a node.
func (p *Profile) Collector() prometheus.Collector { return raftCollector{r: p.raft} }

// raftCollector reads the metrics of r at each scrape.
type raftCollector struct{ r *hraft.Raft }

func (raftCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{stateDesc, termDesc, lastIndexDesc, appliedIndexDesc, lastContactDesc, peersDesc} {
		ch <- d
	}
}

func (c raftCollector) Collect(ch chan<- prometheus.Metric) {
	state := c.r.State()
	for _, s := range raftStates {
		v := 0.0
		if s == state {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(stateDesc, prometheus.GaugeValue, v, strings.ToLower(s.String()))
	}
	ch <- prometheus.MustNewConstMetric(termDesc, prometheus.GaugeValue, float64(c.r.CurrentTerm()))
	ch <- prometheus.MustNewConstMetric(lastIndexDesc, prometheus.GaugeValue, float64(c.r.LastIndex()))
	ch <- prometheus.MustNewConstMetric(appliedIndexDesc, prometheus.GaugeValue, float64(c.r.AppliedIndex()))
	if contact := c.r.LastContact(); state == hraft.Follower && !contact.IsZero() {
		ch <- prometheus.MustNewConstMetric(lastContactDesc, prometheus.GaugeValue, time.Since(contact).Seconds())
	}
	if peers, err := strconv.Atoi(c.r.Stats()["num_peers"]); err == nil {
		ch <- prometheus.MustNewConstMetric(peersDesc, prometheus.GaugeValue, float64(peers))
	}
}
