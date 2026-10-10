package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/cranplatform"
	"forge/internal/format/cran"
	"forge/internal/repo"
)

const linuxDigest = "sha256:abababababababababababababababababababababababababababababababab"

// linuxServer wires the plan's topology: a hosted source repo and a proxy in
// the cran-public group, and cran-linux (noOverwrite, sourceRepo=cran-public)
// with noble-x86_64/4.6 registered. upstream serves the proxy.
func linuxServer(t *testing.T, upstream string) *Server {
	t.Helper()
	s := newMigrationServer(t)
	for _, r := range []repo.Repository{
		{Name: "cran-src", Format: "cran", Kind: repo.Hosted, Enabled: true, Claims: []string{"internalpkg"}},
		{Name: "cran-up", Format: "cran", Kind: repo.Proxy, Enabled: true, Upstream: upstream},
		{Name: "cran-public", Format: "cran", Kind: repo.Group, Enabled: true, Members: []string{"cran-src", "cran-up"}},
		{Name: "cran-linux", Format: "cran", Kind: repo.Hosted, Enabled: true, NoOverwrite: true, SourceRepo: "cran-public"},
	} {
		if err := s.Repos.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := cranplatform.NewStore(s.Meta).Put(cranplatform.Platform{
		Key: "noble-x86_64/4.6", RPlatform: "x86_64-pc-linux-gnu", BuilderImage: "img",
		BuilderDigest: linuxDigest, MaxGlibc: "2.39", Libraries: []string{"libc.so.6"},
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func pkgDesc(pkg string) string {
	return "Package: " + pkg + "\nVersion: 1.0\nTitle: T\nLicense: MIT\nNeedsCompilation: no\n"
}

func binaryOf(t *testing.T, pkg string) []byte {
	return tgzFiles(t, map[string]string{
		pkg + "/DESCRIPTION":      pkgDesc(pkg) + "Built: R 4.6.1; ; 2026-10-10 07:19:08 UTC; unix\n",
		pkg + "/Meta/package.rds": "rds",
	})
}

func putLinux(t *testing.T, s *Server, pkg string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut,
		"/repository/cran-linux/bin/linux/noble-x86_64/contrib/4.6/"+pkg+"_1.0.tar.gz", bytes.NewReader(body))
	req.Header.Set(cran.BuilderDigestHeader, linuxDigest)
	rw := httptest.NewRecorder()
	s.Routes().ServeHTTP(rw, req)
	return rw
}

func TestCRANLinux_UploadThroughServer(t *testing.T) {
	s := linuxServer(t, "http://127.0.0.1:1") // proxy unused here
	seedBlobBytes(t, s, http.MethodPut, "cran-src", "src/contrib/lb_1.0.tar.gz",
		tgzWithFile(t, "lb/DESCRIPTION", pkgDesc("lb")))

	if rw := putLinux(t, s, "lb", binaryOf(t, "lb")); rw.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rw.Code, rw.Body)
	}
	if rw := putLinux(t, s, "lb", binaryOf(t, "lb")); rw.Code != http.StatusConflict {
		t.Fatalf("re-upload: %d %s", rw.Code, rw.Body)
	}
	del := httptest.NewRecorder()
	s.Routes().ServeHTTP(del, httptest.NewRequest(http.MethodDelete,
		"/repository/cran-linux/bin/linux/noble-x86_64/contrib/4.6/lb_1.0.tar.gz", nil))
	if del.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", del.Code, del.Body)
	}
	if rw := putLinux(t, s, "lb", binaryOf(t, "lb")); rw.Code != http.StatusCreated {
		t.Fatalf("upload after removal: %d %s", rw.Code, rw.Body)
	}
}

// The source a binary is checked against is read through the group with the
// dependency-confusion guard, exactly as a client would read it: for a name a
// hosted repository claims, upstream's package of that name is not "the
// source forge serves", so a binary of it is refused.
func TestCRANLinux_SourceReadHonoursDepGuard(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pkg := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/src/contrib/"), "_1.0.tar.gz")
		if pkg == "" || strings.Contains(pkg, "/") {
			http.NotFound(w, r)
			return
		}
		w.Write(tgzWithFile(t, pkg+"/DESCRIPTION", pkgDesc(pkg))) //nolint:errcheck
	}))
	defer up.Close()
	s := linuxServer(t, up.URL)

	if rw := putLinux(t, s, "publicpkg", binaryOf(t, "publicpkg")); rw.Code != http.StatusCreated {
		t.Fatalf("unclaimed name via the proxy member: %d %s", rw.Code, rw.Body)
	}
	rw := putLinux(t, s, "internalpkg", binaryOf(t, "internalpkg"))
	if rw.Code == http.StatusCreated {
		t.Fatal("a binary of upstream's package was accepted under a name cran-src claims")
	}
}

// Once a hosted repo holds some version of a name, the guard no longer blocks
// the name outright; it keeps the proxy member out of the group instead, so a
// version only upstream has stays an honest 404. The source read must see the
// same 404.
func TestCRANLinux_SourceReadHonoursMemberFilter(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tgzWithFile(t, "internalpkg/DESCRIPTION", pkgDesc("internalpkg"))) //nolint:errcheck
	}))
	defer up.Close()
	s := linuxServer(t, up.URL)
	seedBlobBytes(t, s, http.MethodPut, "cran-src", "src/contrib/internalpkg_0.5.tar.gz",
		tgzWithFile(t, "internalpkg/DESCRIPTION", "Package: internalpkg\nVersion: 0.5\nLicense: MIT\n"))
	if rw := putLinux(t, s, "internalpkg", binaryOf(t, "internalpkg")); rw.Code == http.StatusCreated {
		t.Fatal("upstream's internalpkg 1.0 was used as the source of an owned name")
	}
}

// A standalone proxy as sourceRepo has no member list to filter; the guard
// refuses claimed names outright there.
func TestCRANLinux_ProxySourceRepoHonoursClaims(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tgzWithFile(t, "internalpkg/DESCRIPTION", pkgDesc("internalpkg"))) //nolint:errcheck
	}))
	defer up.Close()
	s := linuxServer(t, up.URL)
	rp, _ := s.Repos.Get("cran-linux")
	rp.SourceRepo = "cran-up"
	if err := s.Repos.Update(rp); err != nil {
		t.Fatal(err)
	}
	if rw := putLinux(t, s, "internalpkg", binaryOf(t, "internalpkg")); rw.Code == http.StatusCreated {
		t.Fatal("a proxy sourceRepo served upstream's package for a claimed name")
	}
}

func tgzFiles(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(content)) //nolint:errcheck
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}
