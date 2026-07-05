package cran

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
		{"src/contrib/mypkg_1.0.0.tar.gz", "mypkg", true},
		{"src/contrib/Archive/mypkg/mypkg_0.9.tar.gz", "mypkg", true},
		{"bin/windows/contrib/4.3/mypkg_1.0.0.zip", "mypkg", true},
		{"bin/macosx/contrib/4.3/mypkg_1.0.0.tgz", "mypkg", true},
		// not claim targets
		{"src/contrib/PACKAGES", "", false},
		{"src/contrib/PACKAGES.gz", "", false},
		{"bin/windows/contrib/4.3/PACKAGES", "", false},
		{"src/contrib/noversion.tar.gz", "", false},
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
		Repo: repo.Repository{Name: "cran-hosted", Format: "cran", Kind: repo.Hosted},
		Meta: m,
	}
	m.PutJSON("cran-hosted+cran", "mypkg_1.0.0", pkgRecord{Package: "mypkg", Version: "1.0.0"})

	h := New()
	if !h.OwnsComponent(c, "mypkg") {
		t.Error("expected ownership of mypkg")
	}
	// Package names cannot contain "_", so the "mypkg_" prefix is exact:
	// "myp" must not match "mypkg_1.0.0".
	if h.OwnsComponent(c, "myp") {
		t.Error("prefix name must not be owned")
	}
	if h.OwnsComponent(c, "other") {
		t.Error("unpublished package must not be owned")
	}
}
