package cran

import "testing"

// CRAN was the one OSV-scannable format answering "" to OSVEcosystem, so
// every CRAN repository was skipped by vulnerability scanning with no error
// and nothing in the UI to say so. OSV really does carry CRAN advisories (the
// RSEC series), so the omission was silence, not a decision.
func TestOSVSeams(t *testing.T) {
	h := New()
	if eco := h.OSVEcosystem(); eco != "CRAN" {
		t.Errorf("OSVEcosystem = %q, want CRAN", eco)
	}
	eco, name, ok := h.OSVCoordinates("commonmark")
	if !ok || eco != "CRAN" || name != "commonmark" {
		t.Errorf("OSVCoordinates = (%q, %q, %v)", eco, name, ok)
	}
	if _, _, ok := h.OSVCoordinates(""); ok {
		t.Error("OSVCoordinates accepted an empty component")
	}

	for _, tc := range []struct {
		sub       string
		comp, ver string
		wantGated bool
	}{
		{"src/contrib/commonmark_1.9.0.tar.gz", "commonmark", "1.9.0", true},
		{"src/contrib/Archive/jsonlite/jsonlite_1.8.0.tar.gz", "jsonlite", "1.8.0", true},
		{"bin/windows/contrib/4.3/jsonlite_1.8.7.zip", "jsonlite", "1.8.7", true},
		{"bin/macosx/big-sur-arm64/contrib/4.3/jsonlite_1.8.7.tgz", "jsonlite", "1.8.7", true},
		// The index carries no version, and blocking it would break the whole
		// repository rather than one package.
		{"src/contrib/PACKAGES", "", "", false},
		{"src/contrib/PACKAGES.gz", "", "", false},
		{"src/contrib/noversion.tar.gz", "", "", false},
	} {
		comp, ver, ok := h.VulnGateTarget(tc.sub)
		if ok != tc.wantGated || comp != tc.comp || ver != tc.ver {
			t.Errorf("VulnGateTarget(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.sub, comp, ver, ok, tc.comp, tc.ver, tc.wantGated)
		}
	}
}
