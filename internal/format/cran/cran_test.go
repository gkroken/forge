package cran

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
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
		ns := repoName + "+cran"
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

func TestParsePackagesFile(t *testing.T) {
	input := "Package: foo\nVersion: 1.0.0\nLicense: MIT\nImports: bar,\n  baz\n\nPackage: qux\nVersion: 2.1.0\nDepends: R (>= 4.0)\n\n"
	recs := parsePackagesFile([]byte(input))
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	if recs[0].Package != "foo" || recs[0].Version != "1.0.0" || recs[0].License != "MIT" {
		t.Errorf("record 0 wrong: %+v", recs[0])
	}
	if recs[0].Imports != "bar, baz" {
		t.Errorf("continuation line not joined: %q", recs[0].Imports)
	}
	if recs[1].Package != "qux" || recs[1].Depends != "R (>= 4.0)" {
		t.Errorf("record 1 wrong: %+v", recs[1])
	}
}

func TestParsePackagesFile_Empty(t *testing.T) {
	if recs := parsePackagesFile(nil); recs != nil {
		t.Fatalf("expected nil for empty input, got %v", recs)
	}
}

// TestGroup_PackagesMerge_WithProxy verifies that a group repo containing a
// proxy member includes packages from the upstream PACKAGES file in its index.
func TestGroup_PackagesMerge_WithProxy(t *testing.T) {
	// Fake upstream CRAN serving a PACKAGES file.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/src/contrib/PACKAGES" {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "Package: upstream-pkg\nVersion: 3.0.0\nLicense: GPL-3\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	b, _ := blob.NewFS(filepath.Join(dir, "b"))

	// Seed one package directly into the hosted member.
	ns := "cran-hosted+cran"
	if err := m.PutJSON(ns, "local-pkg_1.0.0", pkgRecord{Package: "local-pkg", Version: "1.0.0", License: "MIT"}); err != nil {
		t.Fatal(err)
	}

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "cran-hosted", Format: "cran", Kind: repo.Hosted},
		{Name: "cran-proxy", Format: "cran", Kind: repo.Proxy, Upstream: upstream.URL},
		{Name: "cran-group", Format: "cran", Kind: repo.Group, Members: []string{"cran-hosted", "cran-proxy"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}

	groupRepo, _ := mgr.Get("cran-group")
	c := &format.Context{
		Repo:  groupRepo,
		Meta:  m,
		Blob:  b,
		Repos: mgr,
		HTTP:  upstream.Client(),
	}

	recs := New().groupPkgRecords(c)

	pkgNames := map[string]bool{}
	for _, rec := range recs {
		pkgNames[rec.Package] = true
	}
	if !pkgNames["local-pkg"] {
		t.Error("group PACKAGES missing local hosted package")
	}
	if !pkgNames["upstream-pkg"] {
		t.Error("group PACKAGES missing upstream proxy package")
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
	// Offset 18: element count = 2 rows * 8 cols = 16
	if got := readBEInt32(raw, 18); got != 16 {
		t.Errorf("element count at 18: got %d, want 16 (2 rows * 8 cols)", got)
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

func TestBuildPackages_Binary_Windows_Golden(t *testing.T) {
	recs := []pkgRecord{
		{
			Package: "winpkg", Version: "1.0.0", License: "MIT",
			Built:  "R 4.4.0; x86_64-w64-mingw32; 2024-01-15 00:00:00 UTC; windows",
			Archs:  "x64",
			OStype: "windows",
		},
	}
	golden.Assert(t, buildPackages(recs), "bin_windows_packages.txt")
}

func TestBuildPackages_Binary_macOS_Golden(t *testing.T) {
	recs := []pkgRecord{
		{
			Package: "macpkg", Version: "2.0.0", License: "MIT",
			Built:  "R 4.4.0; aarch64-apple-darwin20; 2024-01-15 00:00:00 UTC; unix",
			OStype: "unix",
		},
	}
	golden.Assert(t, buildPackages(recs), "bin_macos_packages.txt")
}

func TestBuildPackagesRDS_Binary_Windows_Golden(t *testing.T) {
	recs := []pkgRecord{
		{
			Package: "winpkg", Version: "1.0.0", License: "MIT",
			Built:  "R 4.4.0; x86_64-w64-mingw32; 2024-01-15 00:00:00 UTC; windows",
			Archs:  "x64",
			OStype: "windows",
		},
	}
	raw := decompressRDS(t, buildPackagesRDS(recs))
	golden.Assert(t, raw, "bin_windows_packages.rds")
}

// --- HTTP-level tests -------------------------------------------------------

// makeCRANPkg creates a minimal CRAN source tarball containing only a DESCRIPTION.
func makeCRANPkg(t *testing.T, pkg, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	desc := fmt.Sprintf("Package: %s\nVersion: %s\nLicense: MIT\n", pkg, version)
	tw.WriteHeader(&tar.Header{Name: pkg + "/DESCRIPTION", Mode: 0644, Size: int64(len(desc))})
	tw.Write([]byte(desc))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// newCRANCtx returns a hosted CRAN Context backed by temp FS stores.
func newCRANCtx(t *testing.T) *format.Context {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	return &format.Context{
		Repo: repo.Repository{Name: "cran-hosted", Format: "cran", Kind: repo.Hosted},
		Blob: b, Meta: m,
	}
}

func cranServe(c *format.Context, method, sub string, body io.Reader) *httptest.ResponseRecorder {
	c.Sub = sub
	if body == nil {
		body = http.NoBody
	}
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(method, "/", body), c)
	return rw
}

func TestServe_CRAN_PublishAndPACKAGES(t *testing.T) {
	c := newCRANCtx(t)
	pkg := makeCRANPkg(t, "mypackage", "1.0.0")

	// PUT uploads the package.
	rw := cranServe(c, http.MethodPut, "src/contrib/mypackage_1.0.0.tar.gz", bytes.NewReader(pkg))
	if rw.Code != http.StatusCreated {
		t.Fatalf("PUT: got %d, body: %s", rw.Code, rw.Body)
	}

	// GET PACKAGES lists the published package.
	rw = cranServe(c, http.MethodGet, "src/contrib/PACKAGES", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("PACKAGES: got %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "Package: mypackage") {
		t.Fatalf("PACKAGES missing entry: %s", rw.Body)
	}
}

func TestServe_CRAN_PackagesGZ(t *testing.T) {
	c := newCRANCtx(t)
	cranServe(c, http.MethodPut, "src/contrib/pkg_1.0.0.tar.gz", bytes.NewReader(makeCRANPkg(t, "pkg", "1.0.0")))

	rw := cranServe(c, http.MethodGet, "src/contrib/PACKAGES.gz", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("PACKAGES.gz: got %d", rw.Code)
	}
	gz, err := gzip.NewReader(rw.Body)
	if err != nil {
		t.Fatalf("decompressing PACKAGES.gz: %v", err)
	}
	body, _ := io.ReadAll(gz)
	if !strings.Contains(string(body), "Package: pkg") {
		t.Fatalf("PACKAGES.gz missing entry")
	}
}

func TestServe_CRAN_PackagesRDS(t *testing.T) {
	c := newCRANCtx(t)
	cranServe(c, http.MethodPut, "src/contrib/pkg_1.0.0.tar.gz", bytes.NewReader(makeCRANPkg(t, "pkg", "1.0.0")))

	rw := cranServe(c, http.MethodGet, "src/contrib/PACKAGES.rds", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("PACKAGES.rds: got %d", rw.Code)
	}
	if rw.Body.Len() == 0 {
		t.Fatal("PACKAGES.rds empty")
	}
}

func TestServe_CRAN_Download(t *testing.T) {
	c := newCRANCtx(t)
	pkg := makeCRANPkg(t, "mypackage", "1.0.0")
	cranServe(c, http.MethodPut, "src/contrib/mypackage_1.0.0.tar.gz", bytes.NewReader(pkg))

	rw := cranServe(c, http.MethodGet, "src/contrib/mypackage_1.0.0.tar.gz", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("download: got %d", rw.Code)
	}
	if !bytes.Equal(rw.Body.Bytes(), pkg) {
		t.Fatal("downloaded bytes differ from uploaded")
	}
}

func TestServe_CRAN_Download_NotFound(t *testing.T) {
	c := newCRANCtx(t)
	rw := cranServe(c, http.MethodGet, "src/contrib/absent_1.0.0.tar.gz", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

func TestServe_CRAN_PutOnProxy_Rejected(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{Name: "cran-proxy", Format: "cran", Kind: repo.Proxy},
		Blob: b, Meta: m,
	}
	rw := cranServe(c, http.MethodPut, "src/contrib/pkg_1.0.0.tar.gz",
		bytes.NewReader(makeCRANPkg(t, "pkg", "1.0.0")))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rw.Code)
	}
}

func TestServe_CRAN_Proxy_Download(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "mock-cran-content")
	}))
	defer upstream.Close()

	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{
			Name: "cran-proxy", Format: "cran", Kind: repo.Proxy,
			Upstream: upstream.URL,
		},
		Blob: b, Meta: m,
		HTTP: upstream.Client(),
	}
	rw := cranServe(c, http.MethodGet, "src/contrib/R.pkg_1.0.0.tar.gz", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("proxy download: got %d, body: %s", rw.Code, rw.Body)
	}
	if rw.Body.String() != "mock-cran-content" {
		t.Fatalf("unexpected body: %q", rw.Body)
	}
}

func TestBrowseRepo_CRAN(t *testing.T) {
	c := newCRANCtx(t)
	cranServe(c, http.MethodPut, "src/contrib/alpha_1.0.0.tar.gz", bytes.NewReader(makeCRANPkg(t, "alpha", "1.0.0")))
	cranServe(c, http.MethodPut, "src/contrib/beta_2.0.0.tar.gz", bytes.NewReader(makeCRANPkg(t, "beta", "2.0.0")))

	entries, err := New().BrowseRepo(c)
	if err != nil {
		t.Fatalf("BrowseRepo: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(entries), entries)
	}
	if entries[0].Name != "alpha" || entries[1].Name != "beta" {
		t.Fatalf("unexpected entries: %v", entries)
	}
}

func TestServe_CRAN_UnsupportedMethod(t *testing.T) {
	// DELETE is only allowed on hosted repos; proxy should reject with 405.
	c := newCRANCtx(t)
	c.Repo.Kind = repo.Proxy
	rw := cranServe(c, http.MethodDelete, "src/contrib/pkg_1.0.0.tar.gz", nil)
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rw.Code)
	}
}

func TestFormat_CRAN(t *testing.T) {
	if got := New().Format(); got != "cran" {
		t.Fatalf("Format() = %q, want cran", got)
	}
}

// ── Binary tree tests ─────────────────────────────────────────────────────────

func TestParseBinPath(t *testing.T) {
	cases := []struct {
		sub              string
		platform, rver   string
		file             string
		ok               bool
	}{
		{"bin/windows/contrib/4.4/PACKAGES",             "windows",         "4.4", "PACKAGES",           true},
		{"bin/windows/contrib/4.4/mypackage_1.0.0.zip",  "windows",         "4.4", "mypackage_1.0.0.zip", true},
		{"bin/macosx/x86_64/contrib/4.4/pkg_1.0.0.tgz", "macosx/x86_64",   "4.4", "pkg_1.0.0.tgz",      true},
		{"bin/macosx/big-sur-arm64/contrib/4.4/PACKAGES","macosx/big-sur-arm64","4.4","PACKAGES",         true},
		{"bin/macosx/contrib/4.2/pkg_1.0.0.tgz",        "macosx",          "4.2", "pkg_1.0.0.tgz",      true},
		{"src/contrib/pkg_1.0.0.tar.gz",                 "",                "",    "",                   false},
		{"bin/windows/contrib/4.4",                      "windows",         "4.4", "",                   false},
	}
	for _, tc := range cases {
		platform, rver, file, ok := parseBinPath(tc.sub)
		if ok != tc.ok || platform != tc.platform || rver != tc.rver || file != tc.file {
			t.Errorf("parseBinPath(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tc.sub, platform, rver, file, ok,
				tc.platform, tc.rver, tc.file, tc.ok)
		}
	}
}

func TestParsePkgFilename(t *testing.T) {
	cases := []struct {
		file       string
		pkg, ver   string
		ok         bool
	}{
		{"mypackage_1.0.0.zip",   "mypackage", "1.0.0", true},
		{"mypackage_1.0.0.tgz",   "mypackage", "1.0.0", true},
		{"my_pkg_2.1.0.zip",      "my_pkg",    "2.1.0", true},
		{"nounderscore.zip",       "",          "",      false},
	}
	for _, tc := range cases {
		pkg, ver, ok := parsePkgFilename(tc.file)
		if ok != tc.ok || pkg != tc.pkg || ver != tc.ver {
			t.Errorf("parsePkgFilename(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.file, pkg, ver, ok, tc.pkg, tc.ver, tc.ok)
		}
	}
}

func TestParseDescriptionFromZip(t *testing.T) {
	data := makeWindowsBinPkg(t, "ziptest", "2.0.0")
	rec, err := parseDescriptionFromZip(data)
	if err != nil {
		t.Fatalf("parseDescriptionFromZip: %v", err)
	}
	if rec.Package != "ziptest" || rec.Version != "2.0.0" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.Built == "" || rec.Archs == "" || rec.OStype == "" {
		t.Fatalf("binary fields not parsed: Built=%q Archs=%q OStype=%q", rec.Built, rec.Archs, rec.OStype)
	}
}

func TestBinaryTree_WindowsPublishAndIndex(t *testing.T) {
	c := newCRANCtx(t)

	// PUT a Windows binary package.
	pkg := makeWindowsBinPkg(t, "winpkg", "1.0.0")
	rw := cranServe(c, http.MethodPut, "bin/windows/contrib/4.4/winpkg_1.0.0.zip", bytes.NewReader(pkg))
	if rw.Code != http.StatusCreated {
		t.Fatalf("PUT binary: got %d, body: %s", rw.Code, rw.Body)
	}

	// GET PACKAGES — must list the uploaded package.
	rw = cranServe(c, http.MethodGet, "bin/windows/contrib/4.4/PACKAGES", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("GET PACKAGES: got %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "Package: winpkg") {
		t.Fatalf("PACKAGES missing package, got: %s", rw.Body)
	}

	// GET PACKAGES.gz — must decompress to the same content.
	rw = cranServe(c, http.MethodGet, "bin/windows/contrib/4.4/PACKAGES.gz", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("GET PACKAGES.gz: got %d", rw.Code)
	}
	gr, _ := gzip.NewReader(rw.Body)
	plain, _ := io.ReadAll(gr)
	if !strings.Contains(string(plain), "Package: winpkg") {
		t.Fatalf("PACKAGES.gz decompressed missing package")
	}

	// GET the binary file itself.
	rw = cranServe(c, http.MethodGet, "bin/windows/contrib/4.4/winpkg_1.0.0.zip", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("GET binary package: got %d", rw.Code)
	}
	if rw.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("unexpected Content-Type: %s", rw.Header().Get("Content-Type"))
	}
}

func TestBinaryTree_macOSTgzPublishAndIndex(t *testing.T) {
	c := newCRANCtx(t)

	// PUT a macOS binary package (.tgz with Built/OS_type in DESCRIPTION).
	pkg := makeMacOSBinPkg(t, "macpkg", "1.0.0")
	rw := cranServe(c, http.MethodPut, "bin/macosx/x86_64/contrib/4.4/macpkg_1.0.0.tgz", bytes.NewReader(pkg))
	if rw.Code != http.StatusCreated {
		t.Fatalf("PUT macOS binary: got %d, body: %s", rw.Code, rw.Body)
	}

	// PACKAGES index for this platform must list the package.
	rw = cranServe(c, http.MethodGet, "bin/macosx/x86_64/contrib/4.4/PACKAGES", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("GET macOS PACKAGES: got %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "Package: macpkg") {
		t.Fatalf("macOS PACKAGES missing package: %s", rw.Body)
	}

	// Windows PACKAGES must NOT bleed into macOS PACKAGES (platform isolation).
	winPkg := makeWindowsBinPkg(t, "winpkg", "1.0.0")
	cranServe(c, http.MethodPut, "bin/windows/contrib/4.4/winpkg_1.0.0.zip", bytes.NewReader(winPkg))
	rw = cranServe(c, http.MethodGet, "bin/macosx/x86_64/contrib/4.4/PACKAGES", nil)
	if strings.Contains(rw.Body.String(), "winpkg") {
		t.Fatal("Windows package leaked into macOS PACKAGES index")
	}
}

func TestBinaryTree_PublishToNonHostedRejected(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{Name: "cran-proxy", Format: "cran", Kind: repo.Proxy},
		Blob: b, Meta: m,
	}
	pkg := makeWindowsBinPkg(t, "pkg", "1.0.0")
	rw := cranServe(c, http.MethodPut, "bin/windows/contrib/4.4/pkg_1.0.0.zip", bytes.NewReader(pkg))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rw.Code)
	}
}

func TestBinaryTree_FilenameOnlyFallback(t *testing.T) {
	c := newCRANCtx(t)
	// Upload a zip whose DESCRIPTION is missing — forge falls back to filename.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("mypkg/README")
	f.Write([]byte("no DESCRIPTION here"))
	zw.Close()

	rw := cranServe(c, http.MethodPut, "bin/windows/contrib/4.4/mypkg_3.0.0.zip", &buf)
	if rw.Code != http.StatusCreated {
		t.Fatalf("filename fallback: got %d, body: %s", rw.Code, rw.Body)
	}
	rw = cranServe(c, http.MethodGet, "bin/windows/contrib/4.4/PACKAGES", nil)
	if !strings.Contains(rw.Body.String(), "Package: mypkg") {
		t.Fatalf("filename fallback package not indexed: %s", rw.Body)
	}
}

func TestBinaryTree_InvalidPath(t *testing.T) {
	c := newCRANCtx(t)
	rw := cranServe(c, http.MethodGet, "bin/windows/nocontrib/4.4/PACKAGES", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("invalid path: expected 404, got %d", rw.Code)
	}
}

// makeWindowsBinPkg creates a minimal Windows binary package (.zip) with a
// DESCRIPTION file including Built, Archs, and OS_type, matching what a real
// Windows binary package published to CRAN contains.
func makeWindowsBinPkg(t *testing.T, pkg, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	desc := fmt.Sprintf(
		"Package: %s\nVersion: %s\nLicense: MIT\n"+
			"Built: R 4.4.0; x86_64-w64-mingw32; 2024-01-15 00:00:00 UTC; windows\n"+
			"Archs: x64\nOS_type: windows\n",
		pkg, version,
	)
	f, _ := zw.Create(pkg + "/DESCRIPTION")
	f.Write([]byte(desc))
	zw.Close()
	return buf.Bytes()
}

// ── B3a: Proxy binary trees ───────────────────────────────────────────────────

func TestBinaryTree_Proxy_Download(t *testing.T) {
	pkg := makeWindowsBinPkg(t, "proxypkg", "1.0.0")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write(pkg)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{
			Name: "cran-proxy", Format: "cran", Kind: repo.Proxy,
			Upstream: upstream.URL,
		},
		Blob: b, Meta: m,
		HTTP: upstream.Client(),
	}

	sub := "bin/windows/contrib/4.4/proxypkg_1.0.0.zip"

	// First GET — cache miss, fetches from upstream.
	rw := cranServe(c, http.MethodGet, sub, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("cache miss: got %d, body: %s", rw.Code, rw.Body)
	}
	if !bytes.Equal(rw.Body.Bytes(), pkg) {
		t.Fatal("cache miss: body differs from upstream")
	}

	// Second GET — served from blob cache (upstream can be taken offline).
	upstream.Close()
	rw = cranServe(c, http.MethodGet, sub, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("cache hit: got %d", rw.Code)
	}
	if !bytes.Equal(rw.Body.Bytes(), pkg) {
		t.Fatal("cache hit: body differs")
	}
}

func TestBinaryTree_Proxy_PutRejected(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{Name: "cran-proxy", Format: "cran", Kind: repo.Proxy},
		Blob: b, Meta: m,
	}
	rw := cranServe(c, http.MethodPut, "bin/windows/contrib/4.4/pkg_1.0.0.zip",
		bytes.NewReader(makeWindowsBinPkg(t, "pkg", "1.0.0")))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rw.Code)
	}
}

// ── B3b: Group binary fan-out ─────────────────────────────────────────────────

func newGroupBinCtx(t *testing.T) (cA, cB, cGroup *format.Context) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "bin-a", Format: "cran", Kind: repo.Hosted},
		{Name: "bin-b", Format: "cran", Kind: repo.Hosted},
		{Name: "bin-group", Format: "cran", Kind: repo.Group, Members: []string{"bin-a", "bin-b"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	repoA, _ := mgr.Get("bin-a")
	repoB, _ := mgr.Get("bin-b")
	repoGroup, _ := mgr.Get("bin-group")
	cA = &format.Context{Repo: repoA, Blob: b, Meta: m, Repos: mgr}
	cB = &format.Context{Repo: repoB, Blob: b, Meta: m, Repos: mgr}
	cGroup = &format.Context{Repo: repoGroup, Blob: b, Meta: m, Repos: mgr}
	return
}

func TestBinaryTree_Group_PackagesMerge(t *testing.T) {
	cA, cB, cGroup := newGroupBinCtx(t)

	cranServe(cA, http.MethodPut, "bin/windows/contrib/4.4/pkgA_1.0.0.zip", bytes.NewReader(makeWindowsBinPkg(t, "pkgA", "1.0.0")))
	cranServe(cB, http.MethodPut, "bin/windows/contrib/4.4/pkgB_1.0.0.zip", bytes.NewReader(makeWindowsBinPkg(t, "pkgB", "1.0.0")))

	rw := cranServe(cGroup, http.MethodGet, "bin/windows/contrib/4.4/PACKAGES", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("group PACKAGES: got %d", rw.Code)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "Package: pkgA") || !strings.Contains(body, "Package: pkgB") {
		t.Fatalf("group PACKAGES missing packages: %s", body)
	}
}

func TestBinaryTree_Group_PackagesDeduplication(t *testing.T) {
	cA, cB, cGroup := newGroupBinCtx(t)

	// Same package+version in both members — should appear once.
	cranServe(cA, http.MethodPut, "bin/windows/contrib/4.4/shared_1.0.0.zip", bytes.NewReader(makeWindowsBinPkg(t, "shared", "1.0.0")))
	cranServe(cB, http.MethodPut, "bin/windows/contrib/4.4/shared_1.0.0.zip", bytes.NewReader(makeWindowsBinPkg(t, "shared", "1.0.0")))

	rw := cranServe(cGroup, http.MethodGet, "bin/windows/contrib/4.4/PACKAGES", nil)
	body := rw.Body.String()
	if count := strings.Count(body, "Package: shared"); count != 1 {
		t.Fatalf("expected shared to appear once in group PACKAGES, got %d: %s", count, body)
	}
}

func TestBinaryTree_Group_Download(t *testing.T) {
	cA, cB, cGroup := newGroupBinCtx(t)

	pkgA := makeWindowsBinPkg(t, "pkgA", "1.0.0")
	pkgB := makeWindowsBinPkg(t, "pkgB", "1.0.0")
	cranServe(cA, http.MethodPut, "bin/windows/contrib/4.4/pkgA_1.0.0.zip", bytes.NewReader(pkgA))
	cranServe(cB, http.MethodPut, "bin/windows/contrib/4.4/pkgB_1.0.0.zip", bytes.NewReader(pkgB))

	// Group serves pkgA from member A.
	rw := cranServe(cGroup, http.MethodGet, "bin/windows/contrib/4.4/pkgA_1.0.0.zip", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("group download pkgA: got %d", rw.Code)
	}
	if !bytes.Equal(rw.Body.Bytes(), pkgA) {
		t.Fatal("group download pkgA: body mismatch")
	}

	// Group serves pkgB from member B.
	rw = cranServe(cGroup, http.MethodGet, "bin/windows/contrib/4.4/pkgB_1.0.0.zip", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("group download pkgB: got %d", rw.Code)
	}
	if !bytes.Equal(rw.Body.Bytes(), pkgB) {
		t.Fatal("group download pkgB: body mismatch")
	}

	// Package absent in all members → 404.
	rw = cranServe(cGroup, http.MethodGet, "bin/windows/contrib/4.4/absent_1.0.0.zip", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("group download absent: got %d, want 404", rw.Code)
	}
}

func TestBrowseRepo_Group_IncludesBinaryPackages(t *testing.T) {
	cA, cB, cGroup := newGroupBinCtx(t)

	cranServe(cA, http.MethodPut, "bin/windows/contrib/4.4/pkgA_1.0.0.zip", bytes.NewReader(makeWindowsBinPkg(t, "pkgA", "1.0.0")))
	cranServe(cB, http.MethodPut, "bin/windows/contrib/4.4/pkgB_1.0.0.zip", bytes.NewReader(makeWindowsBinPkg(t, "pkgB", "1.0.0")))

	entries, err := New().BrowseRepo(cGroup)
	if err != nil {
		t.Fatalf("BrowseRepo group: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(entries), entries)
	}
	names := entries[0].Name + "," + entries[1].Name
	if !strings.Contains(names, "pkgA") || !strings.Contains(names, "pkgB") {
		t.Fatalf("expected pkgA and pkgB, got: %s", names)
	}
}

// ── B2a: DELETE for binary packages ──────────────────────────────────────────

func TestBinaryTree_Delete(t *testing.T) {
	c := newCRANCtx(t)
	pkg := makeWindowsBinPkg(t, "delpkg", "1.0.0")

	rw := cranServe(c, http.MethodPut, "bin/windows/contrib/4.4/delpkg_1.0.0.zip", bytes.NewReader(pkg))
	if rw.Code != http.StatusCreated {
		t.Fatalf("PUT: got %d", rw.Code)
	}

	// Verify appears in PACKAGES before deletion.
	rw = cranServe(c, http.MethodGet, "bin/windows/contrib/4.4/PACKAGES", nil)
	if !strings.Contains(rw.Body.String(), "Package: delpkg") {
		t.Fatal("package not in PACKAGES before delete")
	}

	// DELETE → 204.
	rw = cranServe(c, http.MethodDelete, "bin/windows/contrib/4.4/delpkg_1.0.0.zip", nil)
	if rw.Code != http.StatusNoContent {
		t.Fatalf("DELETE: got %d, body: %s", rw.Code, rw.Body)
	}

	// GET blob after delete → 404.
	rw = cranServe(c, http.MethodGet, "bin/windows/contrib/4.4/delpkg_1.0.0.zip", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("GET after delete: got %d, want 404", rw.Code)
	}

	// PACKAGES no longer lists it.
	rw = cranServe(c, http.MethodGet, "bin/windows/contrib/4.4/PACKAGES", nil)
	if strings.Contains(rw.Body.String(), "Package: delpkg") {
		t.Fatal("deleted package still appears in PACKAGES")
	}
}

func TestBinaryTree_Delete_NotFound(t *testing.T) {
	c := newCRANCtx(t)
	rw := cranServe(c, http.MethodDelete, "bin/windows/contrib/4.4/absent_1.0.0.zip", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("DELETE absent: got %d, want 404", rw.Code)
	}
}

func TestBinaryTree_Delete_NonHostedRejected(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{Name: "cran-proxy", Format: "cran", Kind: repo.Proxy},
		Blob: b, Meta: m,
	}
	rw := cranServe(c, http.MethodDelete, "bin/windows/contrib/4.4/pkg_1.0.0.zip", nil)
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rw.Code)
	}
}

// ── B2b: BrowseRepo includes binary packages ──────────────────────────────────

func TestBrowseRepo_IncludesBinaryPackages(t *testing.T) {
	c := newCRANCtx(t)

	// Source package: mypkg 1.0.0.
	cranServe(c, http.MethodPut, "src/contrib/mypkg_1.0.0.tar.gz", bytes.NewReader(makeCRANPkg(t, "mypkg", "1.0.0")))
	// Windows binary: mypkg 2.0.0 (different version — should be a separate entry under same name).
	cranServe(c, http.MethodPut, "bin/windows/contrib/4.4/mypkg_2.0.0.zip", bytes.NewReader(makeWindowsBinPkg(t, "mypkg", "2.0.0")))
	// macOS binary: separate package.
	cranServe(c, http.MethodPut, "bin/macosx/x86_64/contrib/4.4/otherpkg_1.0.0.tgz", bytes.NewReader(makeMacOSBinPkg(t, "otherpkg", "1.0.0")))

	entries, err := New().BrowseRepo(c)
	if err != nil {
		t.Fatalf("BrowseRepo: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(entries), entries)
	}

	var mypkg format.BrowseEntry
	for _, e := range entries {
		if e.Name == "mypkg" {
			mypkg = e
		}
	}
	if mypkg.Name == "" {
		t.Fatal("mypkg missing from BrowseRepo entries")
	}
	if len(mypkg.Versions) != 2 {
		t.Fatalf("mypkg: expected 2 versions (1.0.0 source + 2.0.0 binary), got %v", mypkg.Versions)
	}
}

func TestBrowseRepo_BinaryVersionDeduplication(t *testing.T) {
	c := newCRANCtx(t)

	// Same package+version published for both Windows and macOS.
	cranServe(c, http.MethodPut, "bin/windows/contrib/4.4/shared_1.0.0.zip", bytes.NewReader(makeWindowsBinPkg(t, "shared", "1.0.0")))
	cranServe(c, http.MethodPut, "bin/macosx/x86_64/contrib/4.4/shared_1.0.0.tgz", bytes.NewReader(makeMacOSBinPkg(t, "shared", "1.0.0")))

	entries, err := New().BrowseRepo(c)
	if err != nil {
		t.Fatalf("BrowseRepo: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after dedup, got %d: %v", len(entries), entries)
	}
	if len(entries[0].Versions) != 1 {
		t.Fatalf("expected 1 version after dedup, got %v", entries[0].Versions)
	}
}

// ── B2c: Platform enumeration ─────────────────────────────────────────────────

func TestBinaryTree_PlatformVersions(t *testing.T) {
	c := newCRANCtx(t)

	cranServe(c, http.MethodPut, "bin/windows/contrib/4.3/mypkg_1.0.0.zip", bytes.NewReader(makeWindowsBinPkg(t, "mypkg", "1.0.0")))
	cranServe(c, http.MethodPut, "bin/windows/contrib/4.4/mypkg_1.0.0.zip", bytes.NewReader(makeWindowsBinPkg(t, "mypkg", "1.0.0")))

	rw := cranServe(c, http.MethodGet, "bin/windows/contrib/", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("platform versions: got %d", rw.Code)
	}
	if ct := rw.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("Content-Type: got %q, want text/plain", ct)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "4.3") || !strings.Contains(body, "4.4") {
		t.Fatalf("expected both R versions in response, got: %q", body)
	}
}

func TestBinaryTree_PlatformVersions_Empty(t *testing.T) {
	c := newCRANCtx(t)
	rw := cranServe(c, http.MethodGet, "bin/windows/contrib/", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("empty platform versions: got %d, want 200", rw.Code)
	}
	if rw.Body.Len() != 0 {
		t.Fatalf("expected empty body for no packages, got: %q", rw.Body)
	}
}

func TestBinaryTree_PlatformVersions_MultiSegmentPlatform(t *testing.T) {
	c := newCRANCtx(t)
	cranServe(c, http.MethodPut, "bin/macosx/x86_64/contrib/4.4/macpkg_1.0.0.tgz", bytes.NewReader(makeMacOSBinPkg(t, "macpkg", "1.0.0")))

	rw := cranServe(c, http.MethodGet, "bin/macosx/x86_64/contrib/", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("multi-segment platform: got %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "4.4") {
		t.Fatalf("expected 4.4 in response, got: %q", rw.Body)
	}
}

// makeMacOSBinPkg creates a minimal macOS binary package (.tgz) with a
// DESCRIPTION file including Built and OS_type for an arm64 macOS target.
func makeMacOSBinPkg(t *testing.T, pkg, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	desc := fmt.Sprintf(
		"Package: %s\nVersion: %s\nLicense: MIT\n"+
			"Built: R 4.4.0; aarch64-apple-darwin20; 2024-01-15 00:00:00 UTC; unix\n"+
			"OS_type: unix\n",
		pkg, version,
	)
	tw.WriteHeader(&tar.Header{Name: pkg + "/DESCRIPTION", Mode: 0644, Size: int64(len(desc))})
	tw.Write([]byte(desc))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}
