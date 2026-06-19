package obs

import (
	"sort"
	"sync"
	"time"
)

// LatencyTracker keeps a fixed-size circular buffer of recent request
// durations and answers approximate percentile queries. Safe for concurrent use.
type LatencyTracker struct {
	mu  sync.Mutex
	buf []int64 // milliseconds
	n   int     // total observations (wraps at len(buf))
}

// NewLatencyTracker returns a tracker that remembers up to cap samples.
func NewLatencyTracker(cap int) *LatencyTracker {
	return &LatencyTracker{buf: make([]int64, cap)}
}

// Observe records a single request duration.
func (lt *LatencyTracker) Observe(d time.Duration) {
	lt.mu.Lock()
	lt.buf[lt.n%len(lt.buf)] = d.Milliseconds()
	lt.n++
	lt.mu.Unlock()
}

// Quantile returns the p-th percentile (0.0–1.0) in milliseconds.
// Returns 0 if no observations have been recorded yet.
func (lt *LatencyTracker) Quantile(p float64) int64 {
	lt.mu.Lock()
	filled := lt.n
	if filled > len(lt.buf) {
		filled = len(lt.buf)
	}
	if filled == 0 {
		lt.mu.Unlock()
		return 0
	}
	cp := make([]int64, filled)
	copy(cp, lt.buf[:filled])
	lt.mu.Unlock()
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[int(float64(filled-1)*p)]
}

// P50 returns the median latency in milliseconds.
func (lt *LatencyTracker) P50() int64 { return lt.Quantile(0.50) }

// P95 returns the 95th-percentile latency in milliseconds.
func (lt *LatencyTracker) P95() int64 { return lt.Quantile(0.95) }

// ThroughputTracker counts requests over a rolling 60-second window using
// per-second buckets. Safe for concurrent use.
type ThroughputTracker struct {
	mu     sync.Mutex
	counts [60]int64
	stamps [60]int64 // unix second for each slot
}

// Inc records one request at the current second.
func (tt *ThroughputTracker) Inc() {
	now := time.Now().Unix()
	i := now % 60
	tt.mu.Lock()
	if tt.stamps[i] != now {
		tt.counts[i] = 0
		tt.stamps[i] = now
	}
	tt.counts[i]++
	tt.mu.Unlock()
}

// RatePerSec returns the average requests-per-second over the last 60 seconds.
func (tt *ThroughputTracker) RatePerSec() float64 {
	now := time.Now().Unix()
	cutoff := now - 59
	var total int64
	tt.mu.Lock()
	for i := range tt.counts {
		if tt.stamps[i] >= cutoff {
			total += tt.counts[i]
		}
	}
	tt.mu.Unlock()
	return float64(total) / 60.0
}
