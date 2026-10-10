package cran

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/cranplatform"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

const (
	uaRocker  = "R/4.6.1 R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)"
	uaRenv    = "renv (1.3.1); R/4.6.1 R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)"
	uaPak     = "R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)"
	uaLibcurl = "libcurl/8.5.0"
	uaR45     = "R/4.5.2 R (4.5.2 x86_64-pc-linux-gnu x86_64 linux-gnu)"
	uaArm     = "R (4.6.1 aarch64-unknown-linux-gnu aarch64 linux-gnu)"
	uaMac     = "R (4.6.1 aarch64-apple-darwin20 aarch64 darwin20)"
)

// serveGroup is cran-public = [cran-src (sources), cran-linux (binaries,
// sourceRepo=cran-public), cran-other (binaries, sourceRepo=elsewhere)].
type serveGroup struct {
	group *format.Context
	src   *format.Context
	bins  *format.Context
	other *format.Context
	srcB  map[string][]byte // source tarball bytes by "pkg_ver"
}

func newServeGroup(t *testing.T) *serveGroup {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "cran-src", Format: "cran", Kind: repo.Hosted},
		{Name: "elsewhere", Format: "cran", Kind: repo.Hosted},
		{Name: "cran-linux", Format: "cran", Kind: repo.Hosted, NoOverwrite: true, SourceRepo: "cran-public"},
		{Name: "cran-other", Format: "cran", Kind: repo.Hosted, NoOverwrite: true, SourceRepo: "elsewhere"},
		{Name: "cran-public", Format: "cran", Kind: repo.Group, Members: []string{"cran-src", "cran-other", "cran-linux"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	reg := cranplatform.NewStore(m)
	if err := reg.Put(lbPlatform()); err != nil {
		t.Fatal(err)
	}
	arm := lbPlatform()
	arm.Key, arm.RPlatform = "noble-aarch64/4.6", "aarch64-unknown-linux-gnu"
	if err := reg.Put(arm); err != nil {
		t.Fatal(err)
	}
	ctx := func(name string) *format.Context {
		r, _ := mgr.Get(name)
		return &format.Context{Repo: r, Blob: b, Meta: m, Repos: mgr}
	}
	g := &serveGroup{group: ctx("cran-public"), src: ctx("cran-src"), bins: ctx("cran-linux"), other: ctx("cran-other"), srcB: map[string][]byte{}}
	return g
}

// source publishes pkg@ver as a source package in cran-src.
func (g *serveGroup) source(t *testing.T, pkg, ver string) {
	t.Helper()
	body := makeCRANPkg(t, pkg, ver)
	g.srcB[pkg+"_"+ver] = body
	if rw := cranServe(g.src, http.MethodPut, "src/contrib/"+pkg+"_"+ver+".tar.gz", bytes.NewReader(body)); rw.Code != http.StatusCreated {
		t.Fatalf("seed source %s %s: %d", pkg, ver, rw.Code)
	}
}

// putBinary stores a binary of pkg@ver under key in c, bypassing upload
// validation (tested elsewhere). Its bytes say what it is.
func putBinary(t *testing.T, c *format.Context, key, pkg, ver string) []byte {
	t.Helper()
	k, _ := cranplatform.ParseKey(key)
	body := []byte("BINARY " + key + " " + pkg + "_" + ver)
	sub := "bin/linux/" + k.Distro + "-" + k.Arch + "/contrib/" + k.RMinor + "/" + pkg + "_" + ver + ".tar.gz"
	if _, err := c.Blob.Put(c.Key(sub), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	return body
}

func getUA(c *format.Context, sub, ua string) *httptest.ResponseRecorder {
	cc := *c
	cc.Sub = sub
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	rw := httptest.NewRecorder()
	New().Serve(rw, r, &cc)
	return rw
}

const lx = "__linux__/noble/latest/src/contrib/"

func TestParseRUserAgent(t *testing.T) {
	for ua, want := range map[string]linuxClient{
		uaRocker: {"4.6", "x86_64-pc-linux-gnu", "x86_64"},
		uaRenv:   {"4.6", "x86_64-pc-linux-gnu", "x86_64"},
		uaPak:    {"4.6", "x86_64-pc-linux-gnu", "x86_64"},
		uaArm:    {"4.6", "aarch64-unknown-linux-gnu", "aarch64"},
	} {
		if got, ok := parseRUserAgent(ua); !ok || got != want {
			t.Errorf("%q: %+v %v", ua, got, ok)
		}
	}
	for _, ua := range []string{
		"", uaLibcurl, "curl/8.5.0", "Wget/1.21.4", uaMac,
		"R (4.6 x86_64-pc-linux-gnu x86_64 linux-gnu)",      // no patch level
		"R (4.6.1 x86_64-pc-linux-gnu aarch64 linux-gnu)",   // arch disagrees with triple
		"NotR (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)", // not R's own token
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36",
	} {
		if got, ok := parseRUserAgent(ua); ok {
			t.Errorf("%q parsed as %+v", ua, got)
		}
	}
}

func TestLinuxServe_BinaryOrSource(t *testing.T) {
	g := newServeGroup(t)
	g.source(t, "lb", "1.0.2")
	bin := putBinary(t, g.bins, "noble-x86_64/4.6", "lb", "1.0.2")
	putBinary(t, g.other, "noble-x86_64/4.5", "lb", "1.0.2")
	putBinary(t, g.bins, "jammy-x86_64/4.6", "lb", "1.0.2") // jammy is not registered
	src := g.srcB["lb_1.0.2"]

	cases := []struct {
		name, sub, ua string
		want          []byte
	}{
		{"rocker install.packages", lx + "lb_1.0.2.tar.gz", uaRocker, bin},
		{"renv", lx + "lb_1.0.2.tar.gz", uaRenv, bin},
		{"pak", lx + "lb_1.0.2.tar.gz", uaPak, bin},
		{"stock install.packages (libcurl)", lx + "lb_1.0.2.tar.gz", uaLibcurl, src},
		{"no User-Agent", lx + "lb_1.0.2.tar.gz", "", src},
		{"other R minor", lx + "lb_1.0.2.tar.gz", uaR45, src},
		{"arm64, registered, no binary", lx + "lb_1.0.2.tar.gz", uaArm, src},
		{"macOS client", lx + "lb_1.0.2.tar.gz", uaMac, src},
		{"same arch, other R build triple", lx + "lb_1.0.2.tar.gz", "R (4.6.1 x86_64-unknown-linux-gnu x86_64 linux-gnu)", src},
		{"other distro", "__linux__/jammy/latest/src/contrib/lb_1.0.2.tar.gz", uaRocker, src},
		{"unknown distro syntax", "__linux__/Noble/latest/src/contrib/lb_1.0.2.tar.gz", uaRocker, src},
	}
	for _, c := range cases {
		rw := getUA(g.group, c.sub, c.ua)
		if rw.Code != 200 || !bytes.Equal(rw.Body.Bytes(), c.want) {
			t.Errorf("%s: %d, got %.40q", c.name, rw.Code, rw.Body.Bytes())
		}
		wantType := "source"
		if bytes.Equal(c.want, bin) {
			wantType = "binary"
		}
		if got := rw.Header().Get("X-Forge-Package-Type"); got != wantType {
			t.Errorf("%s: X-Forge-Package-Type %q, want %q", c.name, got, wantType)
		}
		if rw.Header().Get("Vary") != "User-Agent" {
			t.Errorf("%s: no Vary: User-Agent", c.name)
		}
	}

	// A binary whose repository was validated against another source repo is
	// never served here, even for the key it was built for.
	g2 := newServeGroup(t)
	g2.source(t, "lb", "1.0.2")
	putBinary(t, g2.other, "noble-x86_64/4.6", "lb", "1.0.2")
	if rw := getUA(g2.group, lx+"lb_1.0.2.tar.gz", uaRocker); rw.Header().Get("X-Forge-Package-Type") != "source" {
		t.Error("served a binary from a member whose sourceRepo is not this group")
	}
	// Anything but a group serves source on this path.
	if rw := getUA(g.bins, lx+"lb_1.0.2.tar.gz", uaRocker); rw.Header().Get("X-Forge-Package-Type") == "binary" {
		t.Error("a hosted repository served a binary on the __linux__ path")
	}
}

func TestLinuxServe_ArchiveAndIndex(t *testing.T) {
	g := newServeGroup(t)
	g.source(t, "lb", "0.9")
	g.source(t, "lb", "1.0.2")
	old := putBinary(t, g.bins, "noble-x86_64/4.6", "lb", "0.9")

	if rw := getUA(g.group, lx+"Archive/lb/lb_0.9.tar.gz", uaRocker); !bytes.Equal(rw.Body.Bytes(), old) {
		t.Errorf("archived binary: %d %.40q", rw.Code, rw.Body.Bytes())
	}
	if rw := getUA(g.group, lx+"Archive/other/lb_0.9.tar.gz", uaRocker); rw.Header().Get("X-Forge-Package-Type") == "binary" {
		t.Error("an Archive path naming another package directory served a binary")
	}
	// The index is the source index, whatever the client.
	want := getUA(g.group, "src/contrib/PACKAGES", "").Body.String()
	for _, ua := range []string{uaRocker, uaLibcurl} {
		if got := getUA(g.group, lx+"PACKAGES", ua).Body.String(); got != want || !strings.Contains(got, "Package: lb") {
			t.Errorf("index for %q differs from the source index:\n%s", ua, got)
		}
	}
	// PPM's source view of the repository, which renv derives from the
	// __linux__ URL and records in lockfiles.
	if got := getUA(g.group, "latest/src/contrib/PACKAGES", uaRocker).Body.String(); got != want {
		t.Errorf("latest/src/contrib/PACKAGES differs from the source index:\n%s", got)
	}
	if rw := getUA(g.group, "latest/src/contrib/lb_1.0.2.tar.gz", uaRocker); !bytes.Equal(rw.Body.Bytes(), g.srcB["lb_1.0.2"]) {
		t.Errorf("latest/src/contrib tarball is not the source: %d", rw.Code)
	}
	if rw := getUA(g.group, "__linux__/noble/2026-01-01/src/contrib/PACKAGES", uaRocker); rw.Code != 404 {
		t.Errorf("a snapshot date answered %d; forge has no snapshots", rw.Code)
	}
	r := httptest.NewRequest(http.MethodPut, "/", strings.NewReader("x"))
	cc := *g.bins
	cc.Sub = lx + "lb_9.tar.gz"
	rw := httptest.NewRecorder()
	New().Serve(rw, r, &cc)
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT on the __linux__ view: %d", rw.Code)
	}
	cc.Sub = "latest/src/contrib/lb_9.tar.gz"
	rw = httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(http.MethodPut, "/", strings.NewReader("x")), &cc)
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT on the latest/ view: %d", rw.Code)
	}
}

func pakIndex(t *testing.T, c *format.Context, sub, ua string) string {
	t.Helper()
	rw := getUA(c, sub, ua)
	if rw.Code != 200 {
		t.Fatalf("%s: %d", sub, rw.Code)
	}
	if rw.Header().Get("Vary") != "User-Agent" {
		t.Errorf("%s: no Vary", sub)
	}
	zr, err := gzip.NewReader(rw.Body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(zr)
	return string(b)
}

func TestLinuxServe_PakIndex(t *testing.T) {
	g := newServeGroup(t)
	g.source(t, "full", "1.0")
	g.source(t, "stale", "2.0")
	g.source(t, "multi", "1.0")
	g.source(t, "multi", "1.1")
	putBinary(t, g.bins, "noble-x86_64/4.6", "full", "1.0")
	putBinary(t, g.bins, "noble-x86_64/4.6", "stale", "1.9") // only an older version
	putBinary(t, g.bins, "noble-x86_64/4.6", "multi", "1.0") // one of two indexed versions
	putBinary(t, g.other, "noble-x86_64/4.6", "stale", "2.0")

	const idx = "latest/bin/linux/4.6-noble/contrib/4.6/PACKAGES.gz"
	got := pakIndex(t, g.group, idx, uaPak)
	if !strings.Contains(got, "Package: full") {
		t.Errorf("full not listed:\n%s", got)
	}
	for _, p := range []string{"stale", "multi"} {
		if strings.Contains(got, "Package: "+p+"\n") {
			t.Errorf("%s listed although an indexed version would be served as source:\n%s", p, got)
		}
	}
	// Each listed package downloads as a binary for the same client.
	if rw := getUA(g.group, lx+"full_1.0.tar.gz", uaPak); rw.Header().Get("X-Forge-Package-Type") != "binary" {
		t.Error("listed package does not download as a binary")
	}
	for name, c := range map[string]struct{ sub, ua string }{
		"stock R":            {idx, uaLibcurl},
		"path/UA R mismatch": {"latest/bin/linux/4.5-noble/contrib/4.5/PACKAGES.gz", uaPak},
		"other distro":       {"latest/bin/linux/4.6-jammy/contrib/4.6/PACKAGES.gz", uaPak},
	} {
		if got := pakIndex(t, g.group, c.sub, c.ua); strings.Contains(got, "Package:") {
			t.Errorf("%s: listed %s", name, got)
		}
	}
	if rw := getUA(g.group, "latest/bin/linux/4.6-noble/contrib/4.5/PACKAGES.gz", uaPak); rw.Code != 404 {
		t.Errorf("inconsistent pak index path: %d", rw.Code)
	}
}

// The detail view of a binary repository lists its builds; a group lists the
// builds it serves, and not those of a member validated against other source.
func TestLinuxInspect_ListsBuilds(t *testing.T) {
	g := newServeGroup(t)
	g.source(t, "lb", "1.0.2")
	putBinary(t, g.bins, "noble-x86_64/4.6", "lb", "1.0.2")
	g.bins.Meta.PutJSON(New().binNS(g.bins, "linux/noble-x86_64", "4.6"), "lb_1.0.2", pkgRecord{ //nolint:errcheck
		Package: "lb", Version: "1.0.2", Title: "A Test Package",
		Linux: &linuxProvenance{Uploader: "builder", BuilderImage: "img", SourceRepo: "cran-public", SHA256: "abc", Needs: []string{"libR.so"}},
	})
	putBinary(t, g.other, "noble-x86_64/4.6", "lb", "1.0.2")

	d, ok := New().Inspect(g.bins, "http://f", "lb")
	if !ok || len(d.Versions) != 1 || d.Description != "A Test Package" {
		t.Fatalf("binary repo detail: %v %+v", ok, d)
	}
	b := d.Versions[0].Builds
	if len(b) != 1 || b[0].Platform != "noble-x86_64/4.6" || b[0].SHA256 != "abc" ||
		strings.Join(b[0].SystemRequirements, ",") != "libR.so" || b[0].Provenance[0].Value != "builder" ||
		b[0].DownloadURL != "http://f/repository/cran-linux/bin/linux/noble-x86_64/contrib/4.6/lb_1.0.2.tar.gz" {
		t.Errorf("builds: %+v", b)
	}
	if d.Versions[0].DownloadURL != b[0].DownloadURL {
		t.Errorf("a binary-only version should download its build, got %s", d.Versions[0].DownloadURL)
	}

	gd, ok := New().Inspect(g.group, "http://f", "lb")
	if !ok || len(gd.Versions) != 1 || len(gd.Versions[0].Builds) != 1 ||
		!strings.Contains(gd.Versions[0].Builds[0].DownloadURL, "/cran-linux/") {
		t.Errorf("group detail builds: %+v", gd.Versions)
	}
	if !strings.Contains(gd.Versions[0].DownloadURL, "/repository/cran-public/src/contrib/") {
		t.Errorf("a version with source keeps its source download: %s", gd.Versions[0].DownloadURL)
	}
}

// caseFoldMeta behaves like meta.FS on a case-insensitive filesystem.
type caseFoldMeta struct{ meta.Store }

func (m caseFoldMeta) GetJSON(ns, key string, v any) (bool, error) {
	return m.Store.GetJSON(ns, strings.ToLower(key), v)
}

func (m caseFoldMeta) PutJSON(ns, key string, v any) error {
	return m.Store.PutJSON(ns, strings.ToLower(key), v)
}

// On a case-insensitive store "Noble-x86_64/4.6" finds noble's registration;
// the decision must still treat Noble as an unregistered distro.
func TestLinuxServe_CaseInsensitiveStoreIsNotAKeyMatch(t *testing.T) {
	g := newServeGroup(t)
	g.source(t, "lb", "1.0.2")
	putBinary(t, g.bins, "noble-x86_64/4.6", "lb", "1.0.2")
	g.group.Meta = caseFoldMeta{g.group.Meta}
	rw := getUA(g.group, "__linux__/Noble/latest/src/contrib/lb_1.0.2.tar.gz", uaRocker)
	if rw.Header().Get("X-Forge-Package-Type") != "source" {
		t.Errorf("a differently-cased distro was served noble's binary")
	}
	if rw := getUA(g.group, lx+"lb_1.0.2.tar.gz", uaRocker); rw.Header().Get("X-Forge-Package-Type") != "binary" {
		t.Errorf("the registered distro lost its binary on a case-folding store")
	}
}
