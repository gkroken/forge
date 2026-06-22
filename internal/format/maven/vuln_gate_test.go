package maven

import "testing"

func TestVulnGateTarget(t *testing.T) {
	h := &Handler{}
	tests := []struct {
		sub       string
		comp, ver string
		ok        bool
	}{
		{"com/example/foo/1.2.3/foo-1.2.3.jar", "com.example:foo", "1.2.3", true},
		{"org/apache/logging/log4j/log4j-core/2.14.1/log4j-core-2.14.1.jar",
			"org.apache.logging.log4j:log4j-core", "2.14.1", true},
		{"com/example/foo/1.2.3/foo-1.2.3.war", "com.example:foo", "1.2.3", true},
		// not primary artifacts → not gated
		{"com/example/foo/1.2.3/foo-1.2.3.pom", "", "", false},          // POM
		{"com/example/foo/1.2.3/foo-1.2.3.jar.sha1", "", "", false},     // checksum
		{"com/example/foo/1.2.3/foo-1.2.3.jar.asc", "", "", false},      // signature
		{"com/example/foo/maven-metadata.xml", "", "", false},          // metadata, no version dir
		{"com/example/foo", "", "", false},                             // too short
	}
	for _, tc := range tests {
		comp, ver, ok := h.VulnGateTarget(tc.sub)
		if ok != tc.ok || comp != tc.comp || ver != tc.ver {
			t.Errorf("VulnGateTarget(%q) = (%q,%q,%v) want (%q,%q,%v)",
				tc.sub, comp, ver, ok, tc.comp, tc.ver, tc.ok)
		}
	}
}
