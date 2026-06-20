package obs

import (
	"sync"
	"sync/atomic"
	"time"
)

// hourBucket is one slot in the 24-slot hourly request ring buffer.
type hourBucket struct {
	mu          sync.Mutex
	resetAt     time.Time
	requests    uint64
	cacheHits   uint64
	cacheMisses uint64
}

// minBucket is one slot in the 32-slot 15-minute metrics ring buffer.
type minBucket struct {
	mu         sync.Mutex
	resetAt    time.Time
	requests   uint64
	errors     uint64
	latencySum uint64 // milliseconds total (for avg approx)
}

// GlobalStats collects server-wide metrics for the Dashboard and Observability
// pages. All methods are safe for concurrent use. Stats reset on server restart.
type GlobalStats struct {
	hourly  [24]hourBucket
	metrics [32]minBucket

	// Status class counters (reset on restart).
	TwoXX   atomic.Uint64
	ThreeXX atomic.Uint64
	FourXX  atomic.Uint64
	FiveXX  atomic.Uint64

	// MetaLatencyMS tracks meta store access latency via an EMA.
	MetaLatencyMS *EMATracker
}

func NewGlobalStats() *GlobalStats {
	return &GlobalStats{MetaLatencyMS: NewEMATracker(0.1)}
}

func (gs *GlobalStats) recordHour(now time.Time, f func(*hourBucket)) {
	h := now.Hour()
	b := &gs.hourly[h]
	b.mu.Lock()
	if b.resetAt.IsZero() || now.Sub(b.resetAt) >= time.Hour {
		b.resetAt = now.Truncate(time.Hour)
		b.requests, b.cacheHits, b.cacheMisses = 0, 0, 0
	}
	f(b)
	b.mu.Unlock()
}

func (gs *GlobalStats) recordMin(now time.Time, f func(*minBucket)) {
	slot := (now.Unix() / 900) % 32
	b := &gs.metrics[slot]
	b.mu.Lock()
	if b.resetAt.IsZero() || now.Sub(b.resetAt) >= 15*time.Minute {
		b.resetAt = now.Truncate(15 * time.Minute)
		b.requests, b.errors, b.latencySum = 0, 0, 0
	}
	f(b)
	b.mu.Unlock()
}

// RecordRequest records one completed HTTP request into both ring buffers and
// the status-class counters. durMs is the handler duration in milliseconds.
func (gs *GlobalStats) RecordRequest(statusCode int, durMs int64) {
	now := time.Now()
	gs.recordHour(now, func(b *hourBucket) { b.requests++ })
	gs.recordMin(now, func(b *minBucket) {
		b.requests++
		if statusCode >= 500 {
			b.errors++
		}
		if durMs > 0 {
			b.latencySum += uint64(durMs)
		}
	})
	switch {
	case statusCode == 304:
		gs.ThreeXX.Add(1)
	case statusCode >= 500:
		gs.FiveXX.Add(1)
	case statusCode >= 400:
		gs.FourXX.Add(1)
	default:
		gs.TwoXX.Add(1)
	}
}

// RecordCacheHit notes that the current request was served from the proxy cache.
func (gs *GlobalStats) RecordCacheHit() {
	gs.recordHour(time.Now(), func(b *hourBucket) { b.cacheHits++ })
}

// RecordCacheMiss notes that the current request required an upstream fetch.
func (gs *GlobalStats) RecordCacheMiss() {
	gs.recordHour(time.Now(), func(b *hourBucket) { b.cacheMisses++ })
}

// ── Snapshots ────────────────────────────────────────────────────────────────

// HourlyRequestBucket is one slot in the request-chart response.
type HourlyRequestBucket struct {
	Hour        int    `json:"hour"`
	Requests    uint64 `json:"requests"`
	CacheHits   uint64 `json:"cache_hits"`
	CacheMisses uint64 `json:"cache_misses"`
}

// RequestChartSnapshot returns 24 hourly buckets ordered by hour (0–23).
// Slots not written within the past 24 hours contribute zero counts.
func (gs *GlobalStats) RequestChartSnapshot() []HourlyRequestBucket {
	now := time.Now()
	out := make([]HourlyRequestBucket, 24)
	for h := 0; h < 24; h++ {
		b := &gs.hourly[h]
		b.mu.Lock()
		var req, hits, misses uint64
		if !b.resetAt.IsZero() && now.Sub(b.resetAt) < 24*time.Hour {
			req, hits, misses = b.requests, b.cacheHits, b.cacheMisses
		}
		b.mu.Unlock()
		out[h] = HourlyRequestBucket{Hour: h, Requests: req, CacheHits: hits, CacheMisses: misses}
	}
	return out
}

// MetricsBucket is one slot in the metrics-chart response.
type MetricsBucket struct {
	Bucket  int     `json:"bucket"`   // 0=oldest … 31=newest (current slot)
	ReqRate float64 `json:"req_rate"` // requests per second averaged over 15 min
	AvgMs   float64 `json:"avg_ms"`   // average handler duration in milliseconds
	Errors  uint64  `json:"errors"`
}

// MetricsChartSnapshot returns 32 15-minute buckets ordered oldest-to-newest.
func (gs *GlobalStats) MetricsChartSnapshot() []MetricsBucket {
	now := time.Now()
	currentSlot := (now.Unix() / 900) % 32
	out := make([]MetricsBucket, 32)
	for i := 0; i < 32; i++ {
		slot := (currentSlot - int64(31-i) + 32) % 32
		b := &gs.metrics[slot]
		b.mu.Lock()
		var req, errs, latSum uint64
		if !b.resetAt.IsZero() && now.Sub(b.resetAt) < 8*time.Hour {
			req, errs, latSum = b.requests, b.errors, b.latencySum
		}
		b.mu.Unlock()
		var rate, avgMs float64
		if req > 0 {
			rate = float64(req) / 900.0
			avgMs = float64(latSum) / float64(req)
		}
		out[i] = MetricsBucket{Bucket: i, ReqRate: rate, AvgMs: avgMs, Errors: errs}
	}
	return out
}

// StatusBreakdownEntry is one row in the status-breakdown response.
type StatusBreakdownEntry struct {
	Code  string  `json:"code"`
	Label string  `json:"label"`
	Count uint64  `json:"count"`
	Pct   float64 `json:"pct"`
}

// StatusBreakdown returns 2xx/304/4xx/5xx counts and percentage of total.
func (gs *GlobalStats) StatusBreakdown() []StatusBreakdownEntry {
	two := gs.TwoXX.Load()
	three := gs.ThreeXX.Load()
	four := gs.FourXX.Load()
	five := gs.FiveXX.Load()
	total := two + three + four + five
	pct := func(n uint64) float64 {
		if total == 0 {
			return 0
		}
		return float64(n) / float64(total) * 100
	}
	return []StatusBreakdownEntry{
		{Code: "2xx", Label: "Success", Count: two, Pct: pct(two)},
		{Code: "304", Label: "Not Modified", Count: three, Pct: pct(three)},
		{Code: "4xx", Label: "Client Error", Count: four, Pct: pct(four)},
		{Code: "5xx", Label: "Server Error", Count: five, Pct: pct(five)},
	}
}
