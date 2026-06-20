package obs

import (
	"sync"
	"time"
)

type bucket struct {
	mu           sync.Mutex
	resetAt      time.Time
	hits         uint64
	misses       uint64
	revalidations uint64
	negatives    uint64
}

// RepoStats tracks hourly proxy cache outcomes for one repository using a
// 24-slot ring buffer (one slot per hour-of-day). In-memory only; resets on
// server restart.
type RepoStats struct {
	buckets [24]bucket
}

func (rs *RepoStats) record(now time.Time, f func(*bucket)) {
	h := now.Hour()
	b := &rs.buckets[h]
	b.mu.Lock()
	if b.resetAt.IsZero() || now.Sub(b.resetAt) >= time.Hour {
		b.resetAt = now.Truncate(time.Hour)
		b.hits, b.misses, b.revalidations, b.negatives = 0, 0, 0, 0
	}
	f(b)
	b.mu.Unlock()
}

func (rs *RepoStats) RecordHit()          { rs.record(time.Now(), func(b *bucket) { b.hits++ }) }
func (rs *RepoStats) RecordMiss()         { rs.record(time.Now(), func(b *bucket) { b.misses++ }) }
func (rs *RepoStats) RecordRevalidation() { rs.record(time.Now(), func(b *bucket) { b.revalidations++ }) }
func (rs *RepoStats) RecordNegative()     { rs.record(time.Now(), func(b *bucket) { b.negatives++ }) }

// HourlyBucket is one hour-slot in the 24h stats response.
type HourlyBucket struct {
	Hour   int    `json:"hour"`
	Hits   uint64 `json:"hits"`
	Misses uint64 `json:"misses"`
}

// StatsSnapshot is the JSON shape returned by GET /api/v1/repos/{name}/cache-stats.
type StatsSnapshot struct {
	HitRate24h    float64        `json:"hit_rate_24h"`
	Hourly        []HourlyBucket `json:"hourly"`
	Revalidations uint64         `json:"revalidations"`
	Negatives     uint64         `json:"negatives"`
}

// Snapshot returns a point-in-time view of the ring buffer contents.
// Slots not updated within the past 24 hours contribute zero counts.
func (rs *RepoStats) Snapshot() StatsSnapshot {
	now := time.Now()
	hourly := make([]HourlyBucket, 24)
	var totalHits, totalMisses, totalRevals, totalNegs uint64
	for h := 0; h < 24; h++ {
		b := &rs.buckets[h]
		b.mu.Lock()
		var hits, misses, revals, negs uint64
		if !b.resetAt.IsZero() && now.Sub(b.resetAt) < 24*time.Hour {
			hits, misses, revals, negs = b.hits, b.misses, b.revalidations, b.negatives
		}
		b.mu.Unlock()
		hourly[h] = HourlyBucket{Hour: h, Hits: hits, Misses: misses}
		totalHits += hits
		totalMisses += misses
		totalRevals += revals
		totalNegs += negs
	}
	total := totalHits + totalMisses
	var rate float64
	if total > 0 {
		rate = float64(totalHits) / float64(total)
	}
	return StatsSnapshot{
		HitRate24h:    rate,
		Hourly:        hourly,
		Revalidations: totalRevals,
		Negatives:     totalNegs,
	}
}
