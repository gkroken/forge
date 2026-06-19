package obs

import (
	"sync"
	"testing"
	"time"
)

func TestLatencyTracker_Empty(t *testing.T) {
	lt := NewLatencyTracker(10)
	if got := lt.P50(); got != 0 {
		t.Errorf("want 0 for empty tracker, got %d", got)
	}
}

func TestLatencyTracker_Quantile(t *testing.T) {
	lt := NewLatencyTracker(100)
	for i := 1; i <= 100; i++ {
		lt.Observe(time.Duration(i) * time.Millisecond)
	}
	p50 := lt.P50()
	if p50 < 49 || p50 > 51 {
		t.Errorf("P50 want ~50ms, got %dms", p50)
	}
	p95 := lt.P95()
	if p95 < 94 || p95 > 96 {
		t.Errorf("P95 want ~95ms, got %dms", p95)
	}
}

func TestLatencyTracker_Wraparound(t *testing.T) {
	lt := NewLatencyTracker(10)
	// Fill 20 entries into a 10-slot buffer; last 10 are all 99ms.
	for i := 0; i < 10; i++ {
		lt.Observe(1 * time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		lt.Observe(99 * time.Millisecond)
	}
	if got := lt.P50(); got != 99 {
		t.Errorf("after wraparound P50 want 99ms, got %dms", got)
	}
}

func TestLatencyTracker_Concurrent(t *testing.T) {
	lt := NewLatencyTracker(200)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				lt.Observe(time.Duration(j) * time.Millisecond)
				_ = lt.P50()
			}
		}()
	}
	wg.Wait()
}

func TestThroughputTracker_Zero(t *testing.T) {
	tt := &ThroughputTracker{}
	if got := tt.RatePerSec(); got != 0 {
		t.Errorf("want 0 for empty tracker, got %f", got)
	}
}

func TestThroughputTracker_Count(t *testing.T) {
	tt := &ThroughputTracker{}
	for i := 0; i < 60; i++ {
		tt.Inc()
	}
	rate := tt.RatePerSec()
	if rate < 0.9 || rate > 2.0 {
		t.Errorf("want ~1 req/s after 60 incs, got %f", rate)
	}
}

func TestThroughputTracker_Concurrent(t *testing.T) {
	tt := &ThroughputTracker{}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				tt.Inc()
				_ = tt.RatePerSec()
			}
		}()
	}
	wg.Wait()
}
