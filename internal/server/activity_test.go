package server

import (
	"testing"

	"forge/internal/obs"
)

func TestLeadingVerb(t *testing.T) {
	cases := map[string]string{
		"deleted com.example:app@1.0.0 → trash (1 KB)": "Deleted",
		"restored leftpad@1.2.3 from trash":            "Restored",
		"purged 3 trash entries (5 KB freed)":          "Purged",
		"promote: stage → prod app@1.0 (2 KB)":         "Promoted",
		"quota: blocked write to repo (…)":             "", // keeps method verb
		"":                                             "",
	}
	for detail, want := range cases {
		if got := leadingVerb(detail); got != want {
			t.Errorf("leadingVerb(%q) = %q, want %q", detail, got, want)
		}
	}
}

func TestActivityTarget(t *testing.T) {
	repo := "mvn-h"
	// A curated Detail wins verbatim.
	if got := activityTarget(obs.AuditEntry{Detail: "deleted app@1.0 → trash"}, repo); got != "deleted app@1.0 → trash" {
		t.Errorf("detail not preferred: %q", got)
	}
	// Otherwise the routing prefix is stripped to the artifact coordinates.
	cases := map[string]string{
		"/repository/mvn-h/com/example/app/1.0/app-1.0.jar": "com/example/app/1.0/app-1.0.jar",
		"/v2/mvn-h/manifests/sha256:abc":                    "manifests/sha256:abc",
		"/api/v1/repos/mvn-h/component":                     "component",
	}
	for path, want := range cases {
		if got := activityTarget(obs.AuditEntry{Path: path}, repo); got != want {
			t.Errorf("activityTarget(%q) = %q, want %q", path, got, want)
		}
	}
}
