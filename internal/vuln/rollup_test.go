package vuln_test

import (
	"testing"

	"forge/internal/vuln"
)

func adv(sev vuln.Severity) vuln.Advisory { return vuln.Advisory{ID: "X", Severity: sev} }

func TestBuildRollup_WorstPerComponentAndVersion(t *testing.T) {
	findings := []vuln.Finding{
		{Component: "lodash", Version: "4.17.20", Advisories: []vuln.Advisory{adv(vuln.SeverityHigh), adv(vuln.SeverityLow)}},
		{Component: "lodash", Version: "4.17.21", Advisories: []vuln.Advisory{adv(vuln.SeverityCritical)}},
		{Component: "left-pad", Version: "1.0.0"}, // clean: scanned, no advisories
	}
	r := vuln.BuildRollup("repo", findings)

	if got := r.WorstByComponent["lodash"]; got != vuln.SeverityCritical {
		t.Errorf("worst for lodash = %v, want critical", got)
	}
	if got := r.WorstByVersion["lodash"]["4.17.20"]; got != vuln.SeverityHigh {
		t.Errorf("worst for lodash@4.17.20 = %v, want high", got)
	}
	if got := r.WorstByVersion["lodash"]["4.17.21"]; got != vuln.SeverityCritical {
		t.Errorf("worst for lodash@4.17.21 = %v, want critical", got)
	}
	// Clean component must not appear anywhere.
	if _, ok := r.WorstByComponent["left-pad"]; ok {
		t.Error("clean component left-pad should be absent from WorstByComponent")
	}
}

func TestBuildRollup_HistogramAndCount(t *testing.T) {
	findings := []vuln.Finding{
		{Component: "a", Version: "1", Advisories: []vuln.Advisory{adv(vuln.SeverityCritical)}},
		{Component: "b", Version: "1", Advisories: []vuln.Advisory{adv(vuln.SeverityHigh)}},
		{Component: "c", Version: "1", Advisories: []vuln.Advisory{adv(vuln.SeverityHigh)}},
		{Component: "d", Version: "1"}, // clean
	}
	r := vuln.BuildRollup("repo", findings)

	if r.VulnerableCount != 3 {
		t.Errorf("VulnerableCount = %d, want 3", r.VulnerableCount)
	}
	if r.BySeverity["critical"] != 1 || r.BySeverity["high"] != 2 {
		t.Errorf("BySeverity = %v, want critical:1 high:2", r.BySeverity)
	}
	// Each vulnerable component counted exactly once → histogram sums to count.
	sum := 0
	for _, n := range r.BySeverity {
		sum += n
	}
	if sum != r.VulnerableCount {
		t.Errorf("histogram sums to %d, want %d", sum, r.VulnerableCount)
	}
}

func TestBuildRollup_UnscoredVulnerableStillRecorded(t *testing.T) {
	// A component with advisories but no severity is still vulnerable — it must
	// be recorded (as unknown), not dropped by the zero-value default.
	findings := []vuln.Finding{
		{Component: "mystery", Version: "1", Advisories: []vuln.Advisory{adv(vuln.SeverityUnknown)}},
	}
	r := vuln.BuildRollup("repo", findings)

	if _, ok := r.WorstByComponent["mystery"]; !ok {
		t.Fatal("unscored-but-vulnerable component should be present")
	}
	if r.VulnerableCount != 1 || r.BySeverity["unknown"] != 1 {
		t.Errorf("VulnerableCount=%d BySeverity=%v, want 1 / unknown:1", r.VulnerableCount, r.BySeverity)
	}
}

func TestBuildRollup_Empty(t *testing.T) {
	r := vuln.BuildRollup("repo", nil)
	if r.VulnerableCount != 0 || r.BySeverity != nil || r.WorstByComponent != nil {
		t.Errorf("empty rollup not zero: %+v", r)
	}
}

func TestRollupStore_RoundTrip(t *testing.T) {
	st := newStore(t)
	findings := []vuln.Finding{
		{Component: "lodash", Version: "4.17.20", Source: vuln.SourceOSV, Advisories: []vuln.Advisory{adv(vuln.SeverityHigh)}},
	}
	want := vuln.BuildRollup("npm-hosted", findings)
	if err := st.PutRollup("npm-hosted", want); err != nil {
		t.Fatalf("PutRollup: %v", err)
	}

	got, ok, err := st.GetRollup("npm-hosted")
	if err != nil || !ok {
		t.Fatalf("GetRollup ok=%v err=%v", ok, err)
	}
	if got.VulnerableCount != 1 || got.WorstByComponent["lodash"] != vuln.SeverityHigh {
		t.Errorf("round-trip lost data: %+v", got)
	}
	if got.WorstByVersion["lodash"]["4.17.20"] != vuln.SeverityHigh {
		t.Errorf("round-trip lost per-version severity: %+v", got.WorstByVersion)
	}
}

func TestRollupStore_GetMissing(t *testing.T) {
	st := newStore(t)
	if _, ok, err := st.GetRollup("never-scanned"); ok || err != nil {
		t.Errorf("missing rollup: ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestComputeAndPutRollup_FromStoredFindings(t *testing.T) {
	st := newStore(t)
	if err := st.Put("r", vuln.Finding{Component: "a", Version: "1", Advisories: []vuln.Advisory{adv(vuln.SeverityCritical)}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Put("r", vuln.Finding{Component: "b", Version: "1"}); err != nil { // clean
		t.Fatal(err)
	}

	r, err := st.ComputeAndPutRollup("r")
	if err != nil {
		t.Fatalf("ComputeAndPutRollup: %v", err)
	}
	if r.VulnerableCount != 1 || r.WorstByComponent["a"] != vuln.SeverityCritical {
		t.Errorf("computed rollup wrong: %+v", r)
	}
	// And it was persisted.
	got, ok, _ := st.GetRollup("r")
	if !ok || got.VulnerableCount != 1 {
		t.Errorf("rollup not persisted: ok=%v got=%+v", ok, got)
	}
}
