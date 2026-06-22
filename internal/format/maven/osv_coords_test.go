package maven

import "testing"

func TestOSVCoordinates(t *testing.T) {
	h := New()

	eco, name, ok := h.OSVCoordinates("com.example:foo")
	if !ok || eco != "Maven" || name != "com.example:foo" {
		t.Fatalf("OSVCoordinates(com.example:foo) = (%q,%q,%v)", eco, name, ok)
	}

	// Without the groupId:artifactId separator it isn't a maven coordinate.
	if _, _, ok := h.OSVCoordinates("nogroup"); ok {
		t.Error(`component without ":" should not map`)
	}
}
