package cran

import (
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/format"
	"forge/internal/golden"
	"forge/internal/meta"
	"forge/internal/repo"
)

func TestBuildPackages_Golden(t *testing.T) {
	recs := []pkgRecord{
		{Package: "mathutils", Version: "0.2.0", Imports: "stats", License: "MIT"},
		{Package: "strtools", Version: "1.0.0", License: "GPL-2"},
	}
	golden.Assert(t, buildPackages(recs), "packages_two_pkgs.txt")
}

func TestBuildPackages_Empty(t *testing.T) {
	if got := buildPackages(nil); len(got) != 0 {
		t.Fatalf("expected empty output, got %q", got)
	}
}

func TestScanDescription(t *testing.T) {
	desc := []byte("Package: foo\nVersion: 1.2.3\nImports: bar,\n  baz\nLicense: MIT\n")
	rec := scanDescription(desc)
	if rec.Package != "foo" || rec.Version != "1.2.3" || rec.License != "MIT" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.Imports != "bar, baz" {
		t.Fatalf("continuation line not joined: %q", rec.Imports)
	}
}

// TestGroup_PackagesMerge verifies that a group repo merges PACKAGES records
// from all members and deduplicates overlapping Package+Version pairs.
func TestGroup_PackagesMerge(t *testing.T) {
	dir := t.TempDir()
	m, _ := meta.NewFS(filepath.Join(dir, "m"))

	seed := func(repoName, pkg, version string) {
		ns := repoName + ":cran"
		rec := pkgRecord{Package: pkg, Version: version, License: "MIT"}
		if err := m.PutJSON(ns, pkg+"_"+version, rec); err != nil {
			t.Fatal(err)
		}
	}

	seed("cran-a", "mathutils", "0.1.0")
	seed("cran-a", "mathutils", "0.2.0")
	seed("cran-b", "mathutils", "0.2.0") // duplicate
	seed("cran-b", "strtools", "1.0.0")

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "cran-a", Format: "cran", Kind: repo.Hosted},
		{Name: "cran-b", Format: "cran", Kind: repo.Hosted},
		{Name: "cran-group", Format: "cran", Kind: repo.Group, Members: []string{"cran-a", "cran-b"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}

	groupRepo, _ := mgr.Get("cran-group")
	c := &format.Context{Repo: groupRepo, Meta: m, Repos: mgr}

	recs := New().groupPkgRecords(c)
	// Should have: mathutils 0.1.0, mathutils 0.2.0 (deduped), strtools 1.0.0 = 3 total.
	if len(recs) != 3 {
		t.Errorf("expected 3 records, got %d", len(recs))
	}

	pkgs := string(buildPackages(recs))
	for _, want := range []string{"mathutils", "0.1.0", "0.2.0", "strtools"} {
		if !strings.Contains(pkgs, want) {
			t.Errorf("PACKAGES index missing %q", want)
		}
	}
	// 0.2.0 should appear exactly once.
	if count := strings.Count(pkgs, "0.2.0"); count != 1 {
		t.Errorf("version 0.2.0 appears %d times, want 1", count)
	}
}
