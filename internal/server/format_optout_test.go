package server

import (
	"testing"

	"forge/internal/format"
	"forge/internal/format/oci"
)

// P1 replaced two capability type-assertions with direct calls, on the reasoning
// that format.Unsupported's false answers preserve the old "never guarded" and
// "never gated" behaviour. That was reasoning, not measurement.
func TestUnsupportedDefaults_PreserveOptOut(t *testing.T) {
	h := oci.New()

	if comp, ok := h.ClaimPath("acme/api/blobs/sha256:x"); ok {
		t.Errorf("oci ClaimPath returned (%q, true) — dependency-confusion would now guard a format with no component model", comp)
	}
	if h.OwnsComponent(nil, "acme/api") {
		t.Error("oci OwnsComponent returned true — claims would apply to images")
	}
	// oci DOES implement VulnGateTarget on purpose: it is Trivy-gated, not
	// OSV-gated. The assumption worth pinning is that gating and OSV mapping are
	// independent — a format can have one without the other.
	if _, _, ok := h.VulnGateTarget("acme/api/manifests/v1"); !ok {
		t.Error("oci should still identify a gate target — it is scanned by Trivy")
	}
	if eco := h.OSVEcosystem(); eco != "" {
		t.Errorf("oci OSVEcosystem = %q, want empty", eco)
	}
	var _ format.Handler = h
}
