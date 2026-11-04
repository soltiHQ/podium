package storagetest

import (
	"math"
	"slices"
	"testing"
	"time"
)

// Measure runs op once per iteration and reports the median and the 99th
// percentile of its latency next to the mean the framework prints: a commit
// costs a sync, and one slow sync moves the mean but not the median. Below 100
// iterations the 99th percentile is the slowest one.
func Measure(b *testing.B, op func(i int) error) {
	b.Helper()
	samples := make([]time.Duration, 0, 1024)
	for i := 0; b.Loop(); i++ {
		start := time.Now()
		err := op(i)
		samples = append(samples, time.Since(start))
		if err != nil {
			b.Fatal(err)
		}
	}
	if len(samples) == 0 {
		return
	}
	slices.Sort(samples)
	quantile := func(q float64) float64 {
		rank := max(int(math.Ceil(q*float64(len(samples))))-1, 0)
		return float64(samples[rank]) / float64(time.Millisecond)
	}
	b.ReportMetric(quantile(0.50), "p50-ms")
	b.ReportMetric(quantile(0.99), "p99-ms")
}
