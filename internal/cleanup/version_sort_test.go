package cleanup

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// The core bug: 1.10.0 must outrank 1.9.0 (lexicographic got this wrong).
		{"1.10.0", "1.9.0", 1},
		{"1.9.0", "1.10.0", -1},
		{"2.0.0", "10.0.0", -1},
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		// A release outranks a pre-release of the same core version.
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0", "1.0-SNAPSHOT", 1},
		{"1.0.0-rc1", "1.0.0-rc2", -1},
		// Missing trailing segment ranks lower.
		{"1.0", "1.0.0", -1},
		// Non-numeric segments fall back to lexicographic.
		{"1.0.x", "1.0.y", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
