package cran

import (
	"bytes"
	"compress/gzip"
	"crypto/md5" // #nosec G501 -- CRAN index checksum
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/proxy"
	"forge/internal/repo"
)

func md5hex(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) } // #nosec G401

// fakeCRAN is an upstream that can republish a version with other bytes.
type fakeCRAN struct {
	mu  sync.Mutex
	tar map[string][]byte // filename → bytes
}

func (u *fakeCRAN) set(file string, b []byte) { u.mu.Lock(); u.tar[file] = b; u.mu.Unlock() }

func (u *fakeCRAN) index() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return "Package: other\nVersion: 2.0\nImports: a,\n    b\nMD5sum: " + md5hex(u.tar["other_2.0.tar.gz"]) + "\nNeedsCompilation: no\n\n" +
		"Package: pinned\nVersion: 1.0\nMD5sum: " + md5hex(u.tar["pinned_1.0.tar.gz"]) + "\nLicense: MIT\n\n"
}

func (u *fakeCRAN) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch file := strings.TrimPrefix(r.URL.Path, "/src/contrib/"); file {
	case "PACKAGES":
		io.WriteString(w, u.index()) //nolint:errcheck
	case "PACKAGES.gz":
		gz := gzip.NewWriter(w)
		io.WriteString(gz, u.index()) //nolint:errcheck
		gz.Close()
	case "PACKAGES.rds":
		io.WriteString(w, "UPSTREAM RDS") //nolint:errcheck
	default:
		u.mu.Lock()
		b, ok := u.tar[file]
		u.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b) //nolint:errcheck
	}
}

// A CRAN proxy pins the first bytes of a version, and its index — PACKAGES,
// .gz and .rds — always gives the MD5sum of the file it serves, also after
// upstream republishes that version (forge-issues/008).
func TestProxyPins_IndexDescribesTheServedFile(t *testing.T) {
	v1, v2 := []byte("tarball one"), []byte("tarball TWO, republished")
	up := &fakeCRAN{tar: map[string][]byte{"pinned_1.0.tar.gz": v1, "other_2.0.tar.gz": []byte("other")}}
	srv := httptest.NewServer(up)
	defer srv.Close()

	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	tiny := time.Nanosecond // everything is stale at once, as contentMaxAge: 1s was in the issue
	c := &format.Context{
		Repo: repo.Repository{Name: "cran-proxy", Format: "cran", Kind: repo.Proxy, Upstream: srv.URL,
			ContentMaxAge: &tiny, MetadataMaxAge: &tiny},
		Blob: b, Meta: m, HTTP: srv.Client(),
	}
	get := func(sub string) []byte { return cranServe(c, http.MethodGet, sub, nil).Body.Bytes() }
	gunzip := func(t *testing.T, b []byte) string {
		t.Helper()
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("not gzip: %v", err)
		}
		out, _ := io.ReadAll(zr)
		return string(out)
	}
	md5Of := func(index, pkg string) string {
		_, after, _ := strings.Cut(index, "Package: "+pkg+"\n")
		_, sum, _ := strings.Cut(after, "MD5sum: ")
		return strings.SplitN(sum, "\n", 2)[0]
	}

	// Nothing cached: upstream's own files.
	if got := string(get("src/contrib/PACKAGES")); got != up.index() {
		t.Fatalf("index before any download:\n%s", got)
	}
	if !bytes.Equal(get("src/contrib/pinned_1.0.tar.gz"), v1) {
		t.Fatal("first download")
	}
	// Cached and unchanged upstream: still upstream's own files.
	if got := string(get("src/contrib/PACKAGES.rds")); got != "UPSTREAM RDS" {
		t.Errorf("rds with nothing republished: %q", got)
	}

	up.set("pinned_1.0.tar.gz", v2)
	if !bytes.Equal(get("src/contrib/pinned_1.0.tar.gz"), v1) {
		t.Fatal("a cached version was replaced by upstream's republished bytes")
	}
	idx := string(get("src/contrib/PACKAGES"))
	if md5Of(idx, "pinned") != md5hex(v1) {
		t.Errorf("PACKAGES MD5sum %s, want the served file's %s:\n%s", md5Of(idx, "pinned"), md5hex(v1), idx)
	}
	if want := strings.Replace(up.index(), "MD5sum: "+md5hex(v2), "MD5sum: "+md5hex(v1)+"\nUpstreamMD5sum: "+md5hex(v2), 1); idx != want {
		t.Errorf("PACKAGES changed beyond the one MD5sum:\n got %q\nwant %q", idx, want)
	}
	if gz := gunzip(t, get("src/contrib/PACKAGES.gz")); gz != idx {
		t.Errorf("PACKAGES.gz differs from PACKAGES:\n%s", gz)
	}
	rds := gunzip(t, get("src/contrib/PACKAGES.rds"))
	if !strings.Contains(rds, md5hex(v1)) || !strings.Contains(rds, "UpstreamMD5sum") || !strings.Contains(rds, "a,\nb") {
		t.Errorf("PACKAGES.rds does not carry the served file's MD5sum, upstream's, or lost a continuation line")
	}

	// A pin is only trusted while that file is cached: once it is gone (any
	// delete path), the index is upstream's again and the next download takes
	// upstream's bytes, which the index then describes.
	if err := c.Blob.Delete(c.Key("src/contrib/pinned_1.0.tar.gz")); err != nil {
		t.Fatal(err)
	}
	if got := string(get("src/contrib/PACKAGES")); got != up.index() {
		t.Errorf("index after the pinned file was deleted:\n%s", got)
	}
	if !bytes.Equal(get("src/contrib/pinned_1.0.tar.gz"), v2) {
		t.Fatal("download after delete")
	}
	if got := md5Of(string(get("src/contrib/PACKAGES")), "pinned"); got != md5hex(v2) {
		t.Errorf("after re-fetch: MD5sum %s, want %s", got, md5hex(v2))
	}
}

// A tarball cached before the fetcher recorded checksums is hashed from the
// store when it is next served.
func TestProxyPins_LegacyCacheEntry(t *testing.T) {
	up := &fakeCRAN{tar: map[string][]byte{"pinned_1.0.tar.gz": []byte("old bytes"), "other_2.0.tar.gz": []byte("o")}}
	srv := httptest.NewServer(up)
	defer srv.Close()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{Repo: repo.Repository{Name: "cran-proxy", Format: "cran", Kind: repo.Proxy, Upstream: srv.URL},
		Blob: b, Meta: m, HTTP: srv.Client()}
	// What an older forge left behind: the blob and a cache entry without checksums.
	key := c.Key("src/contrib/pinned_1.0.tar.gz")
	b.Put(key, bytes.NewReader([]byte("old bytes")))                                              //nolint:errcheck
	m.PutJSON("cran-proxy:proxy", key, proxy.CacheEntry{FetchedAt: time.Now(), ContentType: "x"}) //nolint:errcheck

	cranServe(c, http.MethodGet, "src/contrib/pinned_1.0.tar.gz", nil)
	if got := New().loadPins(c).MD5["pinned_1.0"]; got != md5hex([]byte("old bytes")) {
		t.Errorf("legacy pin %q", got)
	}
}
