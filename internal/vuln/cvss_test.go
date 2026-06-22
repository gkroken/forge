package vuln

import "testing"

func TestCVSSBaseScore(t *testing.T) {
	cases := []struct {
		vector string
		want   float64
		ok     bool
	}{
		// Canonical "worst case" — all High, network, no privileges/interaction.
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8, true},
		// Reflected XSS archetype, scope changed.
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N", 6.1, true},
		// No impact → 0.0 (still a valid vector).
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N", 0.0, true},
		// v3.0 prefix is also accepted.
		{"CVSS:3.0/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8, true},
		// v4.0 not scored here.
		{"CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N", 0, false},
		// Missing required metric.
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H", 0, false},
		// Not a CVSS vector.
		{"garbage", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := cvssBaseScore(c.vector)
		if ok != c.ok {
			t.Errorf("cvssBaseScore(%q) ok=%v, want %v", c.vector, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("cvssBaseScore(%q) = %v, want %v", c.vector, got, c.want)
		}
	}
}

func TestCVSSToSeverity(t *testing.T) {
	score, ok := cvssBaseScore("CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:H/A:H")
	if !ok {
		t.Fatal("expected ok")
	}
	if got := SeverityFromCVSS(score); got != SeverityHigh {
		t.Errorf("score %v → %v, want high", score, got)
	}
}
