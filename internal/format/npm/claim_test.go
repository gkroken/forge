package npm

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
		{"lodash", "lodash", true},
		{"@acme/foo", "@acme/foo", true},
		{"@acme%2Ffoo", "@acme/foo", true}, // scoped name, percent-encoded slash
		{"lodash/-/lodash-4.17.20.tgz", "lodash", true},
		{"@acme/foo/-/foo-1.0.0.tgz", "@acme/foo", true},
		// not claim targets
		{"-/ping", "", false},
		{"-/package/lodash/dist-tags", "", false},
		{"", "", false},
		{"@acme", "", false}, // a bare scope is not a package
		{"@acme/foo/bar", "", false},
		{"lodash/4.17.20", "", false},
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
		Repo: repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted},
		Meta: m,
	}
	m.PutJSON("npm-hosted:npm", "@acme/foo", map[string]any{"name": "@acme/foo"})

	h := New()
	if !h.OwnsComponent(c, "@acme/foo") {
		t.Error("expected ownership of @acme/foo")
	}
	if h.OwnsComponent(c, "@acme/bar") {
		t.Error("unpublished package must not be owned")
	}
}
