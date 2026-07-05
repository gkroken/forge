package helm

import (
	"path/filepath"
	"testing"

	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

func TestClaimPath(t *testing.T) {
	h := New()
	tests := []struct {
		sub  string
		comp string
		ok   bool
	}{
		{"mychart-1.2.3.tgz", "mychart", true},
		{"my-chart-1.2.3.tgz", "my-chart", true},
		{"my-chart-1.0-beta.tgz", "my-chart", true},
		{"api/charts/mychart", "mychart", true},
		{"api/charts/mychart/1.2.3", "mychart", true},
		// not claim targets
		{"index.yaml", "", false},
		{"api/charts", "", false},
		{"noversion.tgz", "", false},
	}
	for _, tc := range tests {
		comp, ok := h.ClaimPath(tc.sub)
		if ok != tc.ok || comp != tc.comp {
			t.Errorf("ClaimPath(%q) = (%q,%v) want (%q,%v)", tc.sub, comp, ok, tc.comp, tc.ok)
		}
	}
}

func TestOwnsComponent(t *testing.T) {
	dir := t.TempDir()
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted},
		Meta: m,
	}
	m.PutJSON("helm-hosted:helm", "my-chart-1.0.0",
		chartRecord{Name: "my-chart", Version: "1.0.0"})

	h := New()
	if !h.OwnsComponent(c, "my-chart") {
		t.Error("expected ownership of my-chart")
	}
	// "my" prefix-matches the record key "my-chart-1.0.0" but is a different chart.
	if h.OwnsComponent(c, "my") {
		t.Error("prefix name must not be owned")
	}
	if h.OwnsComponent(c, "other") {
		t.Error("unpublished chart must not be owned")
	}
}
