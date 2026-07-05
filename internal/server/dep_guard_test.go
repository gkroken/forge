package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/maven"
	"forge/internal/format/npm"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/repo"
)

// upstreamCounter is a fake npm registry that records which package paths were
// requested, so tests can prove protected names never reach upstream.
type upstreamCounter struct {
	mu   sync.Mutex
	hits map[string]int
	srv  *httptest.Server
}

func newNpmUpstream(t *testing.T) *upstreamCounter {
	t.Helper()
	u := &upstreamCounter{hits: map[string]int{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits[r.URL.Path]++
		u.mu.Unlock()
		if strings.Contains(r.URL.Path, "/-/") { // tarball
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("upstream-tarball-bytes"))
			return
		}
		pkg := strings.TrimPrefix(r.URL.Path, "/")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"name":%q,"dist-tags":{"latest":"9.9.9"},"versions":{"9.9.9":{"name":%q,"version":"9.9.9","dist":{"tarball":"%s/%s/-/pkg-9.9.9.tgz"}}}}`,
			pkg, pkg, u.srv.URL, pkg)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstreamCounter) total(substr string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for p, c := range u.hits {
		if strings.Contains(p, substr) {
			n += c
		}
	}
	return n
}

// newDepGuardServer builds an eval-mode server with an npm hosted repo
// (claims: @acme/**, left-pad), a proxy to the fake upstream, and a group.
func newDepGuardServer(t *testing.T, upstream string, guardOff bool) *Server {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	reg.Register(npm.New())
	reg.Register(maven.New())

	var guard *bool
	if guardOff {
		f := false
		guard = &f
	}
	for _, r := range []repo.Repository{
		{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, Enabled: true,
			Claims: []string{"@acme/**", "left-pad"}},
		{Name: "npm-proxy", Format: "npm", Kind: repo.Proxy, Upstream: upstream, Enabled: true,
			DepConfusionGuard: guard},
		{Name: "npm-group", Format: "npm", Kind: repo.Group, Enabled: true,
			Members: []string{"npm-hosted", "npm-proxy"}, DepConfusionGuard: guard},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	return New(mgr, reg, b, m, nil)
}

// seedNpmPackage stores a packument (and one tarball blob) in the hosted repo,
// simulating a prior publish.
func seedNpmPackage(t *testing.T, s *Server, pkg, version string) {
	t.Helper()
	base := pkg[strings.LastIndex(pkg, "/")+1:]
	doc := map[string]any{
		"name":      pkg,
		"dist-tags": map[string]any{"latest": version},
		"versions": map[string]any{
			version: map[string]any{
				"name": pkg, "version": version,
				"dist": map[string]any{"tarball": "http://forge/repository/npm-hosted/" + pkg + "/-/" + base + "-" + version + ".tgz"},
			},
		},
	}
	if err := s.Meta.PutJSON("npm-hosted:npm", pkg, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Blob.Put("npm-hosted/"+pkg+"/-/"+base+"-"+version+".tgz",
		strings.NewReader("hosted-tarball")); err != nil {
		t.Fatal(err)
	}
}

func getJSON(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, path, nil))
	var doc map[string]any
	json.Unmarshal(rw.Body.Bytes(), &doc)
	return rw.Code, doc
}

// A name present in a hosted group member is protected automatically: the
// group packument must contain only hosted versions and upstream must never
// be consulted, even though upstream advertises a newer version.
func TestDepGuard_AutoDerivedOwnership_GroupNeverReachesUpstream(t *testing.T) {
	up := newNpmUpstream(t)
	srv := newDepGuardServer(t, up.srv.URL, false)
	seedNpmPackage(t, srv, "is-odd", "1.0.0")
	h := srv.Routes()

	code, doc := getJSON(t, h, "/repository/npm-group/is-odd")
	if code != http.StatusOK {
		t.Fatalf("group packument: got %d", code)
	}
	versions := doc["versions"].(map[string]any)
	if _, ok := versions["1.0.0"]; !ok {
		t.Error("hosted version 1.0.0 missing from group packument")
	}
	if _, ok := versions["9.9.9"]; ok {
		t.Error("upstream version 9.9.9 leaked into group packument for a hosted-owned name")
	}

	// A version only upstream has must 404 — never fetched.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/repository/npm-group/is-odd/-/is-odd-9.9.9.tgz", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("upstream-only version of owned name: got %d, want 404", rw.Code)
	}
	if n := up.total("is-odd"); n != 0 {
		t.Errorf("upstream was consulted %d times for a hosted-owned name", n)
	}
}

// A claimed but never-published name is refused with an explanation instead of
// falling through to upstream.
func TestDepGuard_ClaimedUnpublished_Group403(t *testing.T) {
	up := newNpmUpstream(t)
	srv := newDepGuardServer(t, up.srv.URL, false)
	audit := obs.NewAuditLog(50)
	srv.AuditLog = audit
	reg := prometheus.NewRegistry()
	srv.WithMetrics(obs.NewMetrics(reg), reg)
	h := srv.Routes()

	code, doc := getJSON(t, h, "/repository/npm-group/@acme/newpkg")
	if code != http.StatusForbidden {
		t.Fatalf("claimed unpublished name: got %d, want 403", code)
	}
	if doc["claim"] != "@acme/**" || doc["claimedBy"] != "npm-hosted" {
		t.Errorf("403 body missing claim attribution: %v", doc)
	}
	if n := up.total("@acme"); n != 0 {
		t.Errorf("upstream was consulted %d times for a claimed name", n)
	}

	found := false
	for _, e := range audit.Recent(10) {
		if strings.Contains(e.Detail, "dep-guard: blocked @acme/newpkg") {
			found = true
		}
	}
	if !found {
		t.Error("block not recorded in audit log")
	}
	if v := gatherCounter(t, reg, "forge_depguard_blocked_total"); v != 1 {
		t.Errorf("forge_depguard_blocked_total = %v, want 1", v)
	}
}

// gatherCounter sums a counter family from the registry (avoids the extra
// testutil dependency).
func gatherCounter(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var v float64
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				v += m.GetCounter().GetValue()
			}
		}
	}
	return v
}

// A claimed AND published name serves normally from the hosted member.
func TestDepGuard_ClaimedOwned_ServesHostedOnly(t *testing.T) {
	up := newNpmUpstream(t)
	srv := newDepGuardServer(t, up.srv.URL, false)
	seedNpmPackage(t, srv, "@acme/lib", "1.0.0")
	h := srv.Routes()

	code, doc := getJSON(t, h, "/repository/npm-group/@acme/lib")
	if code != http.StatusOK {
		t.Fatalf("claimed+owned packument: got %d, want 200", code)
	}
	if _, ok := doc["versions"].(map[string]any)["9.9.9"]; ok {
		t.Error("upstream version leaked into claimed+owned packument")
	}

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/repository/npm-group/@acme/lib/-/lib-1.0.0.tgz", nil))
	if rw.Code != http.StatusOK || rw.Body.String() != "hosted-tarball" {
		t.Errorf("hosted tarball via group: got %d %q", rw.Code, rw.Body.String())
	}
	if n := up.total("@acme"); n != 0 {
		t.Errorf("upstream was consulted %d times for a claimed name", n)
	}
}

// Unclaimed, un-owned names still proxy through the group untouched.
func TestDepGuard_UnclaimedName_StillProxies(t *testing.T) {
	up := newNpmUpstream(t)
	srv := newDepGuardServer(t, up.srv.URL, false)
	h := srv.Routes()

	code, doc := getJSON(t, h, "/repository/npm-group/lodash")
	if code != http.StatusOK {
		t.Fatalf("unclaimed packument via group: got %d, want 200", code)
	}
	if _, ok := doc["versions"].(map[string]any)["9.9.9"]; !ok {
		t.Error("upstream version missing from unclaimed packument")
	}
	if up.total("lodash") == 0 {
		t.Error("expected upstream fetch for unclaimed name")
	}
}

// Direct requests to a proxy repo refuse claimed names too — a cached copy is
// still upstream content.
func TestDepGuard_DirectProxy_ClaimedName403(t *testing.T) {
	up := newNpmUpstream(t)
	srv := newDepGuardServer(t, up.srv.URL, false)
	h := srv.Routes()

	code, doc := getJSON(t, h, "/repository/npm-proxy/left-pad")
	if code != http.StatusForbidden {
		t.Fatalf("claimed name via direct proxy: got %d, want 403", code)
	}
	if doc["claim"] != "left-pad" {
		t.Errorf("403 body missing claim: %v", doc)
	}
	if n := up.total("left-pad"); n != 0 {
		t.Errorf("upstream was consulted %d times for a claimed name", n)
	}

	if code, _ := getJSON(t, h, "/repository/npm-proxy/lodash"); code != http.StatusOK {
		t.Errorf("unclaimed name via direct proxy: got %d, want 200", code)
	}
}

// DepConfusionGuard=false restores the unguarded behaviour (merge + fall-through).
func TestDepGuard_Disabled_RestoresFallthrough(t *testing.T) {
	up := newNpmUpstream(t)
	srv := newDepGuardServer(t, up.srv.URL, true)
	seedNpmPackage(t, srv, "is-odd", "1.0.0")
	h := srv.Routes()

	code, doc := getJSON(t, h, "/repository/npm-group/is-odd")
	if code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	versions := doc["versions"].(map[string]any)
	if _, ok := versions["9.9.9"]; !ok {
		t.Error("guard off: upstream version should merge into group packument")
	}
	if code, _ := getJSON(t, h, "/repository/npm-proxy/left-pad"); code != http.StatusOK {
		t.Errorf("guard off: claimed name via proxy got %d, want 200", code)
	}
}

// Maven: an artifact owned by the hosted member must not fall through to the
// proxy member, and a claimed-but-unpublished artifact is refused.
func TestDepGuard_Maven_Group(t *testing.T) {
	var mu sync.Mutex
	upstreamHits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstreamHits++
		mu.Unlock()
		w.Write([]byte("upstream-jar"))
	}))
	defer up.Close()

	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	reg.Register(maven.New())
	for _, r := range []repo.Repository{
		{Name: "mvn-hosted", Format: "maven", Kind: repo.Hosted, Enabled: true,
			Claims: []string{"com/acme/**"}},
		{Name: "mvn-proxy", Format: "maven", Kind: repo.Proxy, Upstream: up.URL, Enabled: true},
		{Name: "mvn-group", Format: "maven", Kind: repo.Group, Enabled: true,
			Members: []string{"mvn-hosted", "mvn-proxy"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(mgr, reg, b, m, nil)
	b.Put("mvn-hosted/com/acme/app/1.0/app-1.0.jar", strings.NewReader("hosted-jar"))
	h := srv.Routes()

	// Owned artifact, version only upstream would have → 404, upstream untouched.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/repository/mvn-group/com/acme/app/9.9/app-9.9.jar", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("upstream-only version of owned artifact: got %d, want 404", rw.Code)
	}

	// Hosted version still serves through the group.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/repository/mvn-group/com/acme/app/1.0/app-1.0.jar", nil))
	if rw.Code != http.StatusOK || rw.Body.String() != "hosted-jar" {
		t.Errorf("hosted artifact via group: got %d %q", rw.Code, rw.Body.String())
	}

	// Claimed, unpublished artifact → 403.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/repository/mvn-group/com/acme/other/1.0/other-1.0.jar", nil))
	if rw.Code != http.StatusForbidden {
		t.Errorf("claimed unpublished artifact: got %d, want 403", rw.Code)
	}

	// Unclaimed artifact proxies fine.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/repository/mvn-group/org/other/lib/1.0/lib-1.0.jar", nil))
	if rw.Code != http.StatusOK || rw.Body.String() != "upstream-jar" {
		t.Errorf("unclaimed artifact via group: got %d %q", rw.Code, rw.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if upstreamHits != 1 {
		t.Errorf("upstream hits = %d, want exactly 1 (the unclaimed artifact)", upstreamHits)
	}
}
