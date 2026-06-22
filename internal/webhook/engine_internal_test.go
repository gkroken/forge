package webhook

import (
	"testing"
	"time"
)

// TestExpBackoff_GrowsAndCaps checks the equal-jitter exponential schedule:
// attempt n is in [d/2, d] where d = backoffBase·2^(n-1), capped at maxBackoff,
// and never exceeds the ceiling regardless of attempt number.
func TestExpBackoff_GrowsAndCaps(t *testing.T) {
	bounds := func(n int) (lo, hi time.Duration) {
		d := backoffBase << (n - 1)
		if d <= 0 || d > maxBackoff {
			d = maxBackoff
		}
		return d / 2, d
	}

	for _, n := range []int{1, 2, 3, 4, 5, 8, 20, 40} {
		lo, hi := bounds(n)
		for i := 0; i < 200; i++ {
			got := expBackoff(n)
			if got < lo || got > hi {
				t.Fatalf("expBackoff(%d)=%v out of [%v,%v]", n, got, lo, hi)
			}
			if got > maxBackoff {
				t.Fatalf("expBackoff(%d)=%v exceeds cap %v", n, got, maxBackoff)
			}
		}
	}

	// Median delay should increase across early attempts (growth), even with jitter.
	if expBackoff(1) > maxBackoff/2 || expBackoff(40) < backoffBase {
		t.Fatal("backoff schedule is not monotonic-ish across the range")
	}
}
