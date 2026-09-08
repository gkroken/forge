package pypi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/format"
	"forge/internal/integrity"
	"forge/internal/ledger"
	"forge/internal/repo"
)

// The optional seams are what the UI, the vulnerability scanner, retention and
// the integrity checker read. None of them fail loudly when wrong — a broken
// seam just makes a feature quietly absent — so each one is pinned here.

func findings(res integrity.Result, kind string) []integrity.Finding {
	var out []integrity.Finding
	for _, f := range res.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// TestSeams_HostedLifecycle walks one release from publish to deletion and
// checks every seam that reads it along the way.
func TestSeams_HostedLifecycle(t *testing.T) {
	h, c := New(), securityCtx(t)

	// A release with two artifacts, which is the normal case: a wheel and an sdist.
	if code := upload(t, h, c, "My.Package", "1.0.0", "my_package-1.0.0-py3-none-any.whl"); code != 200 {
		t.Fatalf("wheel upload: %d", code)
	}
	if code := upload(t, h, c, "My.Package", "1.0.0", "my_package-1.0.0.tar.gz"); code != 200 {
		t.Fatalf("sdist upload: %d", code)
	}
	if code := upload(t, h, c, "My.Package", "0.9.0", "my_package-0.9.0-py3-none-any.whl"); code != 200 {
		t.Fatalf("older upload: %d", code)
	}

	if h.BrowseAsTree() {
		t.Error("BrowseAsTree: pypi projects have no folder hierarchy, so the flat list is the honest view")
	}

	entries, err := h.BrowseRepo(c)
	if err != nil || len(entries) != 1 {
		t.Fatalf("BrowseRepo = %+v, %v; want one project", entries, err)
	}
	if entries[0].Name != "my-package" {
		t.Errorf("BrowseRepo name = %q, want the normalized name", entries[0].Name)
	}
	if len(entries[0].Versions) != 2 {
		t.Errorf("BrowseRepo versions = %v, want 2 distinct releases", entries[0].Versions)
	}

	// Inspect is asked under the spelling a user typed, not the normalized one.
	detail, ok := h.Inspect(c, "http://forge.test", "My.Package")
	if !ok {
		t.Fatal("Inspect: not found under the original spelling")
	}
	if len(detail.Versions) != 3 {
		t.Errorf("Inspect versions = %d, want 3 (one per file)", len(detail.Versions))
	}
	if !strings.Contains(detail.InstallSnippet, "pip install --index-url") {
		t.Errorf("Inspect snippet = %q", detail.InstallSnippet)
	}
	var sawWheel bool
	for _, v := range detail.Versions {
		if v.ContentType == "application/vnd.python.wheel" {
			sawWheel = true
		}
	}
	if !sawWheel {
		t.Error("Inspect: no version reported the wheel content type")
	}

	if !h.OwnsComponent(c, "MY_PACKAGE") {
		t.Error("OwnsComponent: must match under any equivalent spelling")
	}
	if h.OwnsComponent(c, "something-else") {
		t.Error("OwnsComponent: claimed a project it does not hold")
	}

	// ListVersions groups files into releases — retention deletes releases, not files.
	vers, err := h.ListVersions(c)
	if err != nil || len(vers) != 2 {
		t.Fatalf("ListVersions = %+v, %v; want 2 releases", vers, err)
	}
	for _, v := range vers {
		if v.Version == "1.0.0" && len(v.BlobKeys) != 2 {
			t.Errorf("release 1.0.0 has %d blob keys, want 2 (wheel + sdist)", len(v.BlobKeys))
		}
		if v.PublishedAt.IsZero() {
			t.Errorf("release %s has no publish time; age-based retention would skip it", v.Version)
		}
	}

	// The ledger drives age-based retention, and must know about the publish.
	if _, ok := ledger.Load(c.Meta, c.Repo.Name)[ledger.Key("my-package", "1.0.0")]; !ok {
		t.Error("no ledger entry: deleteOlderThanDays would be permanently inert on this release")
	}

	// Download serves the stored bytes.
	w := httptest.NewRecorder()
	c.Sub = "packages/my-package/my_package-1.0.0-py3-none-any.whl"
	h.Serve(w, httptest.NewRequest("GET", "/", nil), c)
	if w.Code != 200 || w.Body.String() != "bytes" {
		t.Errorf("download = %d %q", w.Code, w.Body.String())
	}

	// Deleting one file of a release leaves the release, and its ledger row, alone.
	w2 := httptest.NewRecorder()
	h.Serve(w2, httptest.NewRequest("DELETE", "/", nil), c)
	if w2.Code != 204 {
		t.Fatalf("delete file = %d", w2.Code)
	}
	if _, ok := ledger.Load(c.Meta, c.Repo.Name)[ledger.Key("my-package", "1.0.0")]; !ok {
		t.Error("ledger row dropped while the release still has an sdist")
	}

	// Deleting the last file of the release drops the ledger row too, or the
	// ledger grows rows for releases that no longer exist.
	w3 := httptest.NewRecorder()
	c.Sub = "packages/my-package/my_package-1.0.0.tar.gz"
	h.Serve(w3, httptest.NewRequest("DELETE", "/", nil), c)
	if w3.Code != 204 {
		t.Fatalf("delete last file = %d", w3.Code)
	}
	if _, ok := ledger.Load(c.Meta, c.Repo.Name)[ledger.Key("my-package", "1.0.0")]; ok {
		t.Error("ledger row survived the last file of the release")
	}

	// DeleteVersion removes a whole release and reports the bytes it freed.
	freed, err := h.DeleteVersion(c, "My.Package", "0.9.0")
	if err != nil || freed != int64(len("bytes")) {
		t.Errorf("DeleteVersion freed %d, %v; want %d", freed, err, len("bytes"))
	}
	if left, _ := h.ListVersions(c); len(left) != 0 {
		t.Errorf("ListVersions after deletion = %+v, want none", left)
	}
}

// TestVerifyIntegrity_Hosted covers the three ways a hosted repo can be wrong.
func TestVerifyIntegrity_Hosted(t *testing.T) {
	h, c := New(), securityCtx(t)
	if code := upload(t, h, c, "pkg", "1.0.0", "pkg-1.0.0-py3-none-any.whl"); code != 200 {
		t.Fatalf("upload: %d", code)
	}

	// Healthy to begin with.
	res, err := h.VerifyIntegrity(c, integrity.ModeFull)
	if err != nil || len(res.Findings) != 0 {
		t.Fatalf("clean repo reported %+v, %v", res.Findings, err)
	}

	// A record whose file is gone: the index advertises a link that 404s.
	if err := c.Blob.Delete(c.Key("packages/pkg/pkg-1.0.0-py3-none-any.whl")); err != nil {
		t.Fatal(err)
	}
	res, _ = h.VerifyIntegrity(c, integrity.ModeQuick)
	if len(findings(res, integrity.KindMissing)) != 1 {
		t.Errorf("deleted file: got %+v, want one missing finding", res.Findings)
	}

	// A file no record claims: unreachable through the simple index.
	h2, c2 := New(), securityCtx(t)
	if _, err := c2.Blob.Put(c2.Key("packages/ghost/ghost-1.0.0-py3-none-any.whl"), bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	res, _ = h2.VerifyIntegrity(c2, integrity.ModeQuick)
	if len(findings(res, integrity.KindOrphan)) != 1 {
		t.Errorf("unclaimed blob: got %+v, want one orphan finding", res.Findings)
	}

	// Bytes that no longer hash to what the index promises: pip rejects these.
	h3, c3 := New(), securityCtx(t)
	if code := upload(t, h3, c3, "pkg", "1.0.0", "pkg-1.0.0-py3-none-any.whl"); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	if _, err := c3.Blob.Put(c3.Key("packages/pkg/pkg-1.0.0-py3-none-any.whl"), bytes.NewReader([]byte("tampered"))); err != nil {
		t.Fatal(err)
	}
	res, _ = h3.VerifyIntegrity(c3, integrity.ModeFull)
	if len(findings(res, integrity.KindMismatch)) != 1 {
		t.Errorf("tampered bytes: got %+v, want one mismatch finding", res.Findings)
	}
	// Quick mode does not read bytes, so it must not claim to have checked them.
	res, _ = h3.VerifyIntegrity(c3, integrity.ModeQuick)
	if len(findings(res, integrity.KindMismatch)) != 0 {
		t.Errorf("quick mode reported a checksum mismatch it never read: %+v", res.Findings)
	}
}

// TestSeams_ProxyReportsItsCache — a proxy owns no records, so browse, inspect
// and retention describe what it has actually cached. Reporting the ~600k
// upstream projects, or nothing at all, would both be lies.
func TestSeams_ProxyReportsItsCache(t *testing.T) {
	up := stubUpstream(t)
	h, c := New(), securityCtx(t)
	c.Repo = repo.Repository{Name: "p", Format: "pypi", Kind: repo.Proxy, Upstream: up.URL}
	c.HTTP = http.DefaultClient

	// Nothing cached yet.
	if vers, _ := h.ListVersions(c); len(vers) != 0 {
		t.Errorf("cold proxy reports %+v, want nothing cached", vers)
	}

	// Read the index, then pull one file through it.
	for _, sub := range []string{"simple/six", "packages/six/six-1.16.0-py2.py3-none-any.whl"} {
		w := httptest.NewRecorder()
		c.Sub = sub
		h.Serve(w, httptest.NewRequest("GET", "/", nil), c)
		if w.Code != 200 {
			t.Fatalf("%s: %d", sub, w.Code)
		}
	}

	vers, err := h.ListVersions(c)
	if err != nil || len(vers) != 1 {
		t.Fatalf("ListVersions = %+v, %v; want the one cached release", vers, err)
	}
	if vers[0].Component != "six" || vers[0].Version != "1.16.0" {
		t.Errorf("cached release = %s@%s, want six@1.16.0", vers[0].Component, vers[0].Version)
	}
	if vers[0].PublishedAt.IsZero() {
		t.Error("cached release has no fetch time; age-based cache eviction would skip it")
	}

	entries, _ := h.BrowseRepo(c)
	if len(entries) != 1 || entries[0].Name != "six" {
		t.Errorf("BrowseRepo = %+v, want just the cached project", entries)
	}

	// Eviction frees the bytes and forgets the upstream mapping.
	freed, err := h.DeleteVersion(c, "six", "1.16.0")
	if err != nil || freed == 0 {
		t.Errorf("DeleteVersion freed %d, %v; want the cached bytes", freed, err)
	}
	if vers, _ := h.ListVersions(c); len(vers) != 0 {
		t.Errorf("after eviction: %+v, want empty cache", vers)
	}
	// With the mapping gone, the file is not fetchable until the index is read
	// again — forge never invents an upstream URL.
	w := httptest.NewRecorder()
	c.Sub = "packages/six/six-1.16.0-py2.py3-none-any.whl"
	h.Serve(w, httptest.NewRequest("GET", "/", nil), c)
	if w.Code != 404 {
		t.Errorf("post-eviction fetch = %d, want 404 until the index is re-read", w.Code)
	}
}

// TestVerifyIntegrity_Proxy — a proxy's truth is the shared cache convention,
// not records of its own.
func TestVerifyIntegrity_Proxy(t *testing.T) {
	up := stubUpstream(t)
	h, c := New(), securityCtx(t)
	c.Repo = repo.Repository{Name: "p", Format: "pypi", Kind: repo.Proxy, Upstream: up.URL}
	c.HTTP = http.DefaultClient

	for _, sub := range []string{"simple/six", "packages/six/six-1.16.0-py2.py3-none-any.whl"} {
		w := httptest.NewRecorder()
		c.Sub = sub
		h.Serve(w, httptest.NewRequest("GET", "/", nil), c)
	}
	res, err := h.VerifyIntegrity(c, integrity.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("healthy cache reported %+v", res.Findings)
	}

	// A cached blob with no cache entry is the reportable case: nothing will
	// ever revalidate or evict it. (The reverse — an entry whose blob is gone —
	// is self-healing, since the fetcher re-fetches when the blob is absent.)
	key := c.Key("packages/six/six-1.16.0-py2.py3-none-any.whl")
	if err := c.Meta.Delete(c.Repo.Name+":proxy", key); err != nil {
		t.Fatal(err)
	}
	res, _ = h.VerifyIntegrity(c, integrity.ModeQuick)
	if len(findings(res, integrity.KindOrphan)) == 0 {
		t.Errorf("cached blob with no cache entry went unreported: %+v", res.Findings)
	}
}

// TestFormatKey — the registry key the whole spine dispatches on.
func TestFormatKey(t *testing.T) {
	if got := New().Format(); got != "pypi" {
		t.Errorf("Format() = %q, want pypi", got)
	}
}

var _ format.Handler = (*Handler)(nil)
