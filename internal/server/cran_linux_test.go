package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/auth"
	"forge/internal/cleanup"
	"forge/internal/cranplatform"
	"forge/internal/format/cran"
	"forge/internal/obs"
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

// Provenance names the identity that uploaded — the builder token's own
// description, as the server resolved it — and the detail view shows it.
func TestCRANLinux_ProvenanceNamesTheUploader(t *testing.T) {
	s, store := newAuthServer(t)
	s.Handlers.Register(cran.New())
	for _, r := range []repo.Repository{
		{Name: "cran-src", Format: "cran", Kind: repo.Hosted, Enabled: true, AnonymousRead: true},
		{Name: "cran-public", Format: "cran", Kind: repo.Group, Enabled: true, AnonymousRead: true, Members: []string{"cran-src", "cran-linux"}},
		{Name: "cran-linux", Format: "cran", Kind: repo.Hosted, Enabled: true, AnonymousRead: true, NoOverwrite: true, SourceRepo: "cran-public"},
	} {
		if err := s.Repos.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	cranplatform.NewStore(s.Meta).Put(cranplatform.Platform{ //nolint:errcheck
		Key: "noble-x86_64/4.6", RPlatform: "x86_64-pc-linux-gnu", BuilderImage: "img",
		BuilderDigest: linuxDigest, MaxGlibc: "2.39", Libraries: []string{"libc.so.6"},
	})
	seedBlobBytes(t, s, http.MethodPut, "cran-src", "src/contrib/lb_1.0.tar.gz", tgzWithFile(t, "lb/DESCRIPTION", pkgDesc("lb")))
	_, secret, err := store.Create("ci builder noble", []auth.Grant{{Repo: "cran-linux", Actions: []auth.Action{auth.ActionRead, auth.ActionWrite},
		Selectors: []string{"bin/linux/noble-x86_64/**"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/repository/cran-linux/bin/linux/noble-x86_64/contrib/4.6/lb_1.0.tar.gz", bytes.NewReader(binaryOf(t, "lb")))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set(cran.BuilderDigestHeader, linuxDigest)
	rw := httptest.NewRecorder()
	s.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rw.Code, rw.Body)
	}

	rw = httptest.NewRecorder()
	s.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/ui/browse/cran-public/detail?pkg=lb&ver=1.0", nil))
	var d struct {
		Builds []struct {
			Platform   string `json:"platform"`
			Provenance []struct{ Label, Value string }
		} `json:"builds"`
	}
	json.Unmarshal(rw.Body.Bytes(), &d) //nolint:errcheck
	if len(d.Builds) != 1 || d.Builds[0].Platform != "noble-x86_64/4.6" {
		t.Fatalf("detail builds: %d %s", rw.Code, rw.Body)
	}
	if d.Builds[0].Provenance[0].Value != "ci builder noble" {
		t.Errorf("uploader recorded as %q", d.Builds[0].Provenance[0].Value)
	}
}

// A rejected binary is audited with its reason; the request middleware alone
// audits no 400.
func TestCRANLinux_RejectionIsAudited(t *testing.T) {
	s := linuxServer(t, "http://127.0.0.1:1")
	al := obs.NewAuditLog(16)
	s.WithAuditLog(al)
	seedBlobBytes(t, s, http.MethodPut, "cran-src", "src/contrib/lb_1.0.tar.gz", tgzWithFile(t, "lb/DESCRIPTION", pkgDesc("lb")))
	bad := tgzFiles(t, map[string]string{"lb/DESCRIPTION": pkgDesc("lb"), "lb/Meta/package.rds": "rds"}) // no Built:
	if rw := putLinux(t, s, "lb", bad); rw.Code != http.StatusBadRequest {
		t.Fatalf("upload: %d %s", rw.Code, rw.Body)
	}
	for _, e := range al.Recent(16) {
		if e.Status == http.StatusBadRequest && strings.Contains(e.Detail, "cran-linux-binary") && strings.Contains(e.Detail, "Built") {
			return
		}
	}
	t.Fatalf("no audit row with the rejection reason: %+v", al.Recent(16))
}

// A binary installed through the group's __linux__ URL counts as a download of
// that binary, so lastDownloadedDays on the binary repo keeps what is in use.
func TestCRANLinux_GroupDownloadStampsTheBinary(t *testing.T) {
	s := linuxServer(t, "http://127.0.0.1:1")
	s.Cleanup = cleanup.NewPolicyManager(s.Meta)
	g, _ := s.Repos.Get("cran-public")
	g.Members = append(g.Members, "cran-linux")
	if err := s.Repos.Update(g); err != nil {
		t.Fatal(err)
	}
	seedBlobBytes(t, s, http.MethodPut, "cran-src", "src/contrib/lb_1.0.tar.gz", tgzWithFile(t, "lb/DESCRIPTION", pkgDesc("lb")))
	if rw := putLinux(t, s, "lb", binaryOf(t, "lb")); rw.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rw.Code, rw.Body)
	}
	req := httptest.NewRequest(http.MethodGet, "/repository/cran-public/__linux__/noble/latest/src/contrib/lb_1.0.tar.gz", nil)
	req.Header.Set("User-Agent", "R/4.6.1 R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)")
	rw := httptest.NewRecorder()
	s.Routes().ServeHTTP(rw, req)
	if rw.Header().Get("X-Forge-Package-Type") != "binary" {
		t.Fatalf("not served as a binary: %d %v", rw.Code, rw.Header())
	}
	if !stamped(t, s, "cran-linux/bin/linux/noble-x86_64/contrib/4.6/lb_1.0.tar.gz") {
		t.Error("serving the binary through the group did not stamp the binary")
	}
}
