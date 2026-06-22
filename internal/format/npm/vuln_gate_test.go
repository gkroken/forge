package npm

import "testing"

func TestVulnGateTarget(t *testing.T) {
	h := &Handler{}
	tests := []struct {
		sub       string
		comp, ver string
		ok        bool
	}{
		{"lodash/-/lodash-4.17.20.tgz", "lodash", "4.17.20", true},
		{"@angular/core/-/core-12.0.0.tgz", "@angular/core", "12.0.0", true},
		{"lodash/-/lodash-4.17.20-beta.1.tgz", "lodash", "4.17.20-beta.1", true},
		// not primary artifacts → not gated
		{"lodash", "", "", false},                            // packument
		{"-/package/lodash/dist-tags", "", "", false},        // registry endpoint
		{"-/npm/v1/security/audits/quick", "", "", false},    // audit endpoint
		{"lodash/-/lodash-4.17.20.tgz.sig", "", "", false},   // not a .tgz
		{"lodash/-/", "", "", false},                         // empty filename
	}
	for _, tc := range tests {
		comp, ver, ok := h.VulnGateTarget(tc.sub)
		if ok != tc.ok || comp != tc.comp || ver != tc.ver {
			t.Errorf("VulnGateTarget(%q) = (%q,%q,%v) want (%q,%q,%v)",
				tc.sub, comp, ver, ok, tc.comp, tc.ver, tc.ok)
		}
	}
}
