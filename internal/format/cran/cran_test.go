package cran

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
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

// --- PACKAGES.rds tests ----------------------------------------------------

// decompressRDS decompresses the gzip wrapper and returns raw XDR bytes.
func decompressRDS(t *testing.T, data []byte) []byte {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gr.Close()
	b, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	return b
}

// readBEInt32 reads a big-endian int32 from data at offset.
func readBEInt32(data []byte, offset int) int32 {
	return int32(binary.BigEndian.Uint32(data[offset:]))
}

func TestBuildPackagesRDS_Header(t *testing.T) {
	recs := []pkgRecord{
		{Package: "mathutils", Version: "0.2.0", Imports: "stats", License: "MIT"},
	}
	rds := buildPackagesRDS(recs)
	raw := decompressRDS(t, rds)

	// Check XDR marker.
	if len(raw) < 14 {
		t.Fatalf("too short: %d bytes", len(raw))
	}
	if raw[0] != 'X' || raw[1] != '\n' {
		t.Errorf("expected XDR marker 'X\\n', got %q", raw[:2])
	}
	// Serialization version = 2.
	if v := readBEInt32(raw, 2); v != 2 {
		t.Errorf("expected version 2, got %d", v)
	}
}

func TestBuildPackagesRDS_StructuralBytes(t *testing.T) {
	recs := []pkgRecord{
		{Package: "foo", Version: "1.0", License: "MIT"},
		{Package: "bar", Version: "2.0", Depends: "R (>= 4.0)"},
	}
	raw := decompressRDS(t, buildPackagesRDS(recs))

	// Offset 14: STRSXP|HAS_ATTR type tag = 16|512 = 528 = 0x210
	if got := readBEInt32(raw, 14); got != 528 {
		t.Errorf("type tag at 14: got %d (0x%x), want 528 (STRSXP|HAS_ATTR)", got, got)
	}
	// Offset 18: element count = 2 rows * 5 cols = 10
	if got := readBEInt32(raw, 18); got != 10 {
		t.Errorf("element count at 18: got %d, want 10", got)
	}
}

func TestBuildPackagesRDS_Empty(t *testing.T) {
	rds := buildPackagesRDS(nil)
	raw := decompressRDS(t, rds)
	if len(raw) < 14 {
		t.Fatalf("empty RDS too short")
	}
	// Element count = 0
	if got := readBEInt32(raw, 18); got != 0 {
		t.Errorf("expected 0 elements for empty recs, got %d", got)
	}
}

func TestBuildPackagesRDS_NAForEmptyFields(t *testing.T) {
	// Package with no Depends/Imports/License — those fields should be NA (-1).
	recs := []pkgRecord{{Package: "minimal", Version: "0.1"}}
	raw := decompressRDS(t, buildPackagesRDS(recs))

	// After header (14) + STRSXP type (4) + count (4) = offset 22.
	// Element order (column-major): Package, Version, Depends, Imports, License
	// = "minimal", "0.1", NA, NA, NA
	//
	// "minimal": CHARSXP(9) len(7) "minimal" = 4+4+7 = 15 bytes
	// "0.1":     CHARSXP(9) len(3) "0.1"     = 4+4+3 = 11 bytes
	// NA:        CHARSXP(9) len(-1)           = 4+4   = 8 bytes each

	off := 22
	// Skip "minimal" CHARSXP: 4+4+7=15
	off += 4 + 4 + 7
	// Skip "0.1" CHARSXP: 4+4+3=11
	off += 4 + 4 + 3
	// Next should be NA (Depends): CHARSXP type=9, length=-1
	if got := readBEInt32(raw, off); got != 9 {
		t.Errorf("Depends: expected CHARSXP type 9 at %d, got %d", off, got)
	}
	if got := readBEInt32(raw, off+4); got != -1 {
		t.Errorf("Depends: expected NA length -1 at %d, got %d", off+4, got)
	}
}

func TestBuildPackagesRDS_Golden(t *testing.T) {
	recs := []pkgRecord{
		{Package: "mathutils", Version: "0.2.0", Imports: "stats", License: "MIT"},
		{Package: "strtools", Version: "1.0.0", License: "GPL-2"},
	}
	// Golden-test the decompressed bytes (deterministic, no timestamps).
	raw := decompressRDS(t, buildPackagesRDS(recs))
	golden.Assert(t, raw, "packages_two_pkgs.rds")
}
