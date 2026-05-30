package cran

import (
	"testing"

	"forge/internal/golden"
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
