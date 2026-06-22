package npm

import "testing"

func TestOSVCoordinates(t *testing.T) {
	h := New()

	eco, name, ok := h.OSVCoordinates("lodash")
	if !ok || eco != "npm" || name != "lodash" {
		t.Fatalf("OSVCoordinates(lodash) = (%q,%q,%v)", eco, name, ok)
	}

	// Scoped names map unchanged.
	if eco, name, ok := h.OSVCoordinates("@angular/core"); !ok || eco != "npm" || name != "@angular/core" {
		t.Errorf("scoped: (%q,%q,%v)", eco, name, ok)
	}

	if _, _, ok := h.OSVCoordinates(""); ok {
		t.Error("empty component should not map")
	}
}
