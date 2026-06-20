package obs

import (
	"math"
	"sync/atomic"
	"time"

	"forge/internal/meta"
)

// EMATracker computes an exponential moving average stored as atomic bits.
// Safe for concurrent use via CAS loop.
type EMATracker struct {
	bits  atomic.Uint64 // math.Float64bits(ema_value)
	alpha float64       // smoothing factor (0 < alpha <= 1)
}

func NewEMATracker(alpha float64) *EMATracker {
	return &EMATracker{alpha: alpha}
}

// Record feeds a new sample into the EMA.
func (e *EMATracker) Record(value float64) {
	for {
		old := e.bits.Load()
		prev := math.Float64frombits(old)
		next := e.alpha*value + (1-e.alpha)*prev
		if e.bits.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

// Value returns the current EMA. Returns 0 before any samples are recorded.
func (e *EMATracker) Value() float64 {
	return math.Float64frombits(e.bits.Load())
}

// LatencyStore wraps a meta.Store and records each GetJSON/PutJSON latency
// into an EMATracker. It implements meta.Store in full.
type LatencyStore struct {
	inner meta.Store
	ema   *EMATracker
}

func NewLatencyStore(inner meta.Store, ema *EMATracker) *LatencyStore {
	return &LatencyStore{inner: inner, ema: ema}
}

func (ls *LatencyStore) GetJSON(ns, key string, v any) (bool, error) {
	t := time.Now()
	ok, err := ls.inner.GetJSON(ns, key, v)
	ls.ema.Record(float64(time.Since(t).Microseconds()) / 1000.0)
	return ok, err
}

func (ls *LatencyStore) PutJSON(ns, key string, v any) error {
	t := time.Now()
	err := ls.inner.PutJSON(ns, key, v)
	ls.ema.Record(float64(time.Since(t).Microseconds()) / 1000.0)
	return err
}

func (ls *LatencyStore) List(ns string) ([]string, error)      { return ls.inner.List(ns) }
func (ls *LatencyStore) Delete(ns, key string) error            { return ls.inner.Delete(ns, key) }
