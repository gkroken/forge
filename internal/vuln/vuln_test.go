package vuln_test

import (
	"encoding/json"
	"testing"
	"time"

	"forge/internal/meta"
	"forge/internal/vuln"
)

func TestSeverityOrdering(t *testing.T) {
	// Buckets must compare with plain > (worst is greatest).
	order := []vuln.Severity{
		vuln.SeverityUnknown,
		vuln.SeverityLow,
		vuln.SeverityModerate,
		vuln.SeverityHigh,
		vuln.SeverityCritical,
	}
	for i := 1; i < len(order); i++ {
		if !(order[i] > order[i-1]) {
			t.Fatalf("severity %v should outrank %v", order[i], order[i-1])
		}
	}
}

func TestParseSeverity(t *testing.T) {
	cases := map[string]vuln.Severity{
		"LOW":      vuln.SeverityLow,
		"moderate": vuln.SeverityModerate,
		"medium":   vuln.SeverityModerate, // alias
		"High":     vuln.SeverityHigh,
		"CRITICAL": vuln.SeverityCritical,
		"":         vuln.SeverityUnknown,
		"bogus":    vuln.SeverityUnknown,
	}
	for in, want := range cases {
		if got := vuln.ParseSeverity(in); got != want {
			t.Errorf("ParseSeverity(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSeverityFromCVSS(t *testing.T) {
	cases := []struct {
		score float64
		want  vuln.Severity
	}{
		{0, vuln.SeverityUnknown},
		{0.1, vuln.SeverityLow},
		{3.9, vuln.SeverityLow},
		{4.0, vuln.SeverityModerate},
		{6.9, vuln.SeverityModerate},
		{7.0, vuln.SeverityHigh},
		{8.9, vuln.SeverityHigh},
		{9.0, vuln.SeverityCritical},
		{10.0, vuln.SeverityCritical},
	}
	for _, c := range cases {
		if got := vuln.SeverityFromCVSS(c.score); got != c.want {
			t.Errorf("SeverityFromCVSS(%v) = %v, want %v", c.score, got, c.want)
		}
	}
}

func TestSeverityJSONRoundTrip(t *testing.T) {
	for _, s := range []vuln.Severity{
		vuln.SeverityUnknown, vuln.SeverityLow, vuln.SeverityModerate,
		vuln.SeverityHigh, vuln.SeverityCritical,
	} {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		// Marshals as a human-readable label, not the underlying int.
		if string(b) != `"`+s.String()+`"` {
			t.Errorf("Marshal(%v) = %s, want %q", s, b, s.String())
		}
		var got vuln.Severity
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got != s {
			t.Errorf("round-trip: got %v, want %v", got, s)
		}
	}
}

func TestFindingWorst(t *testing.T) {
	if got := (vuln.Finding{}).Worst(); got != vuln.SeverityUnknown {
		t.Errorf("empty finding Worst() = %v, want unknown", got)
	}
	f := vuln.Finding{Advisories: []vuln.Advisory{
		{ID: "a", Severity: vuln.SeverityLow},
		{ID: "b", Severity: vuln.SeverityHigh},
		{ID: "c", Severity: vuln.SeverityModerate},
	}}
	if got := f.Worst(); got != vuln.SeverityHigh {
		t.Errorf("Worst() = %v, want high", got)
	}
}

func newStore(t *testing.T) *vuln.Store {
	t.Helper()
	m, err := meta.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return vuln.NewStore(m)
}

func TestStoreRoundTrip(t *testing.T) {
	st := newStore(t)
	f := vuln.Finding{
		Component: "lodash",
		Version:   "4.17.20",
		Source:    vuln.SourceOSV,
		Advisories: []vuln.Advisory{{
			ID:       "GHSA-35jh-r3h4-6jhm",
			Aliases:  []string{"CVE-2021-23337"},
			Summary:  "Command injection in lodash",
			Severity: vuln.SeverityHigh,
			CVSS:     "CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:H/A:H",
			FixedIn:  []string{"4.17.21"},
			URL:      "https://github.com/advisories/GHSA-35jh-r3h4-6jhm",
		}},
	}

	if err := st.Put("npm-hosted", f); err != nil {
		t.Fatal(err)
	}

	got, ok, err := st.Get("npm-hosted", "lodash", "4.17.20")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Component != f.Component || got.Version != f.Version || got.Source != f.Source {
		t.Errorf("identity mismatch: %+v", got)
	}
	if len(got.Advisories) != 1 || got.Advisories[0].Severity != vuln.SeverityHigh {
		t.Errorf("advisories not preserved: %+v", got.Advisories)
	}
	if got.ScannedAt.IsZero() {
		t.Error("Put should stamp a zero ScannedAt")
	}

	// Absent finding.
	if _, ok, _ := st.Get("npm-hosted", "lodash", "9.9.9"); ok {
		t.Error("expected ok=false for missing version")
	}
	// Namespacing is per-repo.
	if _, ok, _ := st.Get("other-repo", "lodash", "4.17.20"); ok {
		t.Error("finding leaked across repos")
	}
}

func TestStorePutIsIdempotentOverwrite(t *testing.T) {
	st := newStore(t)
	earlier := time.Now().Add(-time.Hour).UTC()
	st.Put("r", vuln.Finding{Component: "c", Version: "1", Source: vuln.SourceOSV, ScannedAt: earlier})

	// Re-scan: no advisories now, fresh timestamp.
	st.Put("r", vuln.Finding{Component: "c", Version: "1", Source: vuln.SourceOSV})

	got, ok, _ := st.Get("r", "c", "1")
	if !ok {
		t.Fatal("missing after overwrite")
	}
	if len(got.Advisories) != 0 {
		t.Errorf("overwrite did not clear advisories: %+v", got.Advisories)
	}
	if !got.ScannedAt.After(earlier) {
		t.Errorf("ScannedAt not refreshed: %v", got.ScannedAt)
	}

	all, _ := st.List("r")
	if len(all) != 1 {
		t.Errorf("overwrite created a duplicate: %d findings", len(all))
	}
}

func TestStoreListSortedAndScoped(t *testing.T) {
	st := newStore(t)
	st.Put("r", vuln.Finding{Component: "beta", Version: "2.0", Source: vuln.SourceOSV})
	st.Put("r", vuln.Finding{Component: "alpha", Version: "1.0", Source: vuln.SourceOSV})
	st.Put("r", vuln.Finding{Component: "alpha", Version: "0.9", Source: vuln.SourceOSV})
	st.Put("other", vuln.Finding{Component: "gamma", Version: "1.0", Source: vuln.SourceOSV})

	got, err := st.List("r")
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"alpha", "0.9"}, {"alpha", "1.0"}, {"beta", "2.0"}}
	if len(got) != len(want) {
		t.Fatalf("List len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Component != w[0] || got[i].Version != w[1] {
			t.Errorf("List[%d] = %s@%s, want %s@%s", i, got[i].Component, got[i].Version, w[0], w[1])
		}
	}
}

// Scoped npm names contain both "@" and "/"; they must survive a store round-trip
// (meta.Store flattens "/" → "__"), and List must reconstruct from the document,
// not by parsing the key.
func TestStoreScopedNpmName(t *testing.T) {
	st := newStore(t)
	const name = "@angular/core"
	st.Put("r", vuln.Finding{Component: name, Version: "12.0.0", Source: vuln.SourceOSV})

	got, ok, err := st.Get("r", name, "12.0.0")
	if err != nil || !ok {
		t.Fatalf("Get scoped: ok=%v err=%v", ok, err)
	}
	if got.Component != name || got.Version != "12.0.0" {
		t.Errorf("scoped identity mangled: %+v", got)
	}

	list, _ := st.List("r")
	if len(list) != 1 || list[0].Component != name {
		t.Errorf("List scoped: %+v", list)
	}

	if err := st.Delete("r", name, "12.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Get("r", name, "12.0.0"); ok {
		t.Error("Delete did not remove scoped finding")
	}
}
