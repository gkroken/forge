package npm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

// hostedCtx builds a Context for a hosted npm repo backed by temp FS stores.
func hostedCtx(t *testing.T) (*format.Context, blob.Store, meta.Store) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted},
		Blob: b, Meta: m,
	}
	return c, b, m
}

// seedPackument writes a packument directly to the meta store.
func seedPackument(t *testing.T, m meta.Store, repoName, pkg string, versions []string, tags map[string]string) {
	t.Helper()
	ns := repoName + ":npm"
	vers := map[string]any{}
	for _, v := range versions {
		vers[v] = map[string]any{
			"name": pkg, "version": v,
			"dist": map[string]any{
				"tarball": "http://localhost/repository/" + repoName + "/" + pkg + "/-/" + pkg + "-" + v + ".tgz",
			},
		}
	}
	dt := map[string]any{}
	for k, v := range tags {
		dt[k] = v
	}
	doc := map[string]any{"name": pkg, "versions": vers, "dist-tags": dt}
	if err := m.PutJSON(ns, pkg, doc); err != nil {
		t.Fatal(err)
	}
}

// --- dist-tags ------------------------------------------------------------

func TestDistTags_List(t *testing.T) {
	c, _, m := hostedCtx(t)
	seedPackument(t, m, "npm-hosted", "mylib", []string{"1.0.0", "1.1.0"},
		map[string]string{"latest": "1.1.0", "beta": "1.1.0"})

	c.Sub = "-/package/mylib/dist-tags"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	var tags map[string]string
	json.NewDecoder(rw.Body).Decode(&tags)
	if tags["latest"] != "1.1.0" || tags["beta"] != "1.1.0" {
		t.Errorf("unexpected tags: %v", tags)
	}
}

func TestDistTags_SetAndGet(t *testing.T) {
	c, _, m := hostedCtx(t)
	seedPackument(t, m, "npm-hosted", "mylib", []string{"1.0.0", "2.0.0"},
		map[string]string{"latest": "1.0.0"})

	// PUT a new tag.
	c.Sub = "-/package/mylib/dist-tags/next"
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`"2.0.0"`))
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", rw.Code, rw.Body.String())
	}

	// GET the specific tag.
	rw2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	New().Serve(rw2, req2, c)
	var ver string
	json.NewDecoder(rw2.Body).Decode(&ver)
	if ver != "2.0.0" {
		t.Errorf("expected 2.0.0, got %q", ver)
	}
}

func TestDistTags_Delete(t *testing.T) {
	c, _, m := hostedCtx(t)
	seedPackument(t, m, "npm-hosted", "mylib", []string{"1.0.0"},
		map[string]string{"latest": "1.0.0", "old": "1.0.0"})

	c.Sub = "-/package/mylib/dist-tags/old"
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusOK {
		t.Fatalf("DELETE status %d", rw.Code)
	}

	// Confirm "old" tag is gone.
	c.Sub = "-/package/mylib/dist-tags"
	rw2 := httptest.NewRecorder()
	New().Serve(rw2, httptest.NewRequest(http.MethodGet, "/", nil), c)
	var tags map[string]string
	json.NewDecoder(rw2.Body).Decode(&tags)
	if _, exists := tags["old"]; exists {
		t.Error("expected 'old' tag to be removed")
	}
	if tags["latest"] != "1.0.0" {
		t.Error("expected 'latest' tag to survive")
	}
}

func TestDistTags_NotFound(t *testing.T) {
	c, _, _ := hostedCtx(t)
	c.Sub = "-/package/nosuchpkg/dist-tags"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

// --- unpublish ------------------------------------------------------------

func TestUnpublish_WholePackage(t *testing.T) {
	c, b, m := hostedCtx(t)
	seedPackument(t, m, "npm-hosted", "mylib", []string{"1.0.0"}, map[string]string{"latest": "1.0.0"})
	// Seed a tarball blob.
	b.Put("npm-hosted/mylib/-/mylib-1.0.0.tgz", strings.NewReader("tarbytes"))

	c.Sub = "mylib"
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}

	// Packument should be gone.
	var doc map[string]any
	if ok, _ := m.GetJSON("npm-hosted:npm", "mylib", &doc); ok {
		t.Error("expected packument to be deleted")
	}
	// Blob should be gone.
	if _, err := b.Get("npm-hosted/mylib/-/mylib-1.0.0.tgz"); err == nil {
		t.Error("expected tarball blob to be deleted")
	}
}

func TestUnpublish_SingleTarball(t *testing.T) {
	c, b, m := hostedCtx(t)
	seedPackument(t, m, "npm-hosted", "mylib", []string{"1.0.0", "2.0.0"}, map[string]string{"latest": "2.0.0"})
	b.Put("npm-hosted/mylib/-/mylib-1.0.0.tgz", strings.NewReader("tarbytes"))

	c.Sub = "mylib/-/mylib-1.0.0.tgz"
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}

	// Blob gone.
	if _, err := b.Get("npm-hosted/mylib/-/mylib-1.0.0.tgz"); err == nil {
		t.Error("expected tarball blob to be deleted")
	}
	// Version 1.0.0 pruned from packument; 2.0.0 still present.
	var doc map[string]any
	m.GetJSON("npm-hosted:npm", "mylib", &doc)
	versions, _ := doc["versions"].(map[string]any)
	if _, ok := versions["1.0.0"]; ok {
		t.Error("expected version 1.0.0 to be pruned from packument")
	}
	if _, ok := versions["2.0.0"]; !ok {
		t.Error("expected version 2.0.0 to survive")
	}
}

// --- deprecate ------------------------------------------------------------

// Deprecate is a publish PUT with a deprecated field and no _attachments.
func TestDeprecate(t *testing.T) {
	c, _, m := hostedCtx(t)
	seedPackument(t, m, "npm-hosted", "mylib", []string{"1.0.0"}, map[string]string{"latest": "1.0.0"})

	payload := map[string]any{
		"name": "mylib",
		"versions": map[string]any{
			"1.0.0": map[string]any{
				"name": "mylib", "version": "1.0.0",
				"deprecated": "use newlib instead",
				"dist":       map[string]any{"tarball": "http://x/mylib/-/mylib-1.0.0.tgz"},
			},
		},
	}
	body, _ := json.Marshal(payload)

	c.Sub = "mylib"
	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
	req.Host = "localhost:8080"
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
	}

	// Confirm deprecated field was stored.
	var doc map[string]any
	m.GetJSON("npm-hosted:npm", "mylib", &doc)
	versions, _ := doc["versions"].(map[string]any)
	v100, _ := versions["1.0.0"].(map[string]any)
	if v100["deprecated"] != "use newlib instead" {
		t.Errorf("deprecated field not stored, got: %v", v100["deprecated"])
	}
}

// --- misc endpoints -------------------------------------------------------

func TestPing(t *testing.T) {
	c, _, _ := hostedCtx(t)
	c.Sub = "-/ping"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := strings.TrimSpace(rw.Body.String())
	if body != "{}" {
		t.Errorf("expected {}, got %q", body)
	}
}

func TestWhoami(t *testing.T) {
	c, _, _ := hostedCtx(t)
	c.Sub = "-/whoami"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	var resp map[string]string
	json.NewDecoder(rw.Body).Decode(&resp)
	if resp["username"] == "" {
		t.Error("expected non-empty username")
	}
}

func TestLogin(t *testing.T) {
	c, _, _ := hostedCtx(t)
	c.Sub = "-/user/org.couchdb.user:bob"
	body := `{"name":"bob","password":"secret"}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusCreated {
		t.Fatalf("status %d", rw.Code)
	}
	var resp map[string]any
	json.NewDecoder(rw.Body).Decode(&resp)
	if resp["ok"] != true {
		t.Errorf("expected ok:true, got %v", resp)
	}
}

func TestAudit(t *testing.T) {
	c, _, _ := hostedCtx(t)
	c.Sub = "-/npm/v1/security/audits"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	var resp map[string]any
	json.NewDecoder(rw.Body).Decode(&resp)
	meta, _ := resp["metadata"].(map[string]any)
	vulns, _ := meta["vulnerabilities"].(map[string]any)
	if int(vulns["critical"].(float64)) != 0 {
		t.Error("expected zero critical vulnerabilities")
	}
}

// --- group tests (carried over) -------------------------------------------

func TestGroup_PackumentMerge(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))

	seedPackument(t, m, "npm-a", "mylib", []string{"1.0.0", "1.1.0"}, map[string]string{"latest": "1.1.0"})
	seedPackument(t, m, "npm-b", "mylib", []string{"1.1.0", "2.0.0"}, map[string]string{"latest": "2.0.0"})
	seedPackument(t, m, "npm-b", "otherlib", []string{"3.0.0"}, map[string]string{"latest": "3.0.0"})

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "npm-a", Format: "npm", Kind: repo.Hosted},
		{Name: "npm-b", Format: "npm", Kind: repo.Hosted},
		{Name: "npm-group", Format: "npm", Kind: repo.Group, Members: []string{"npm-a", "npm-b"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}

	groupRepo, _ := mgr.Get("npm-group")
	c := &format.Context{Repo: groupRepo, Blob: b, Meta: m, Repos: mgr}

	req := httptest.NewRequest(http.MethodGet, "/repository/npm-group/mylib", nil)
	req.Host = "localhost:8080"
	rw := httptest.NewRecorder()
	New().groupPackument(rw, req, c, "mylib")

	var result map[string]any
	json.NewDecoder(rw.Body).Decode(&result)

	versions, _ := result["versions"].(map[string]any)
	if len(versions) != 3 {
		t.Errorf("expected 3 versions, got %d", len(versions))
	}
	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		if _, ok := versions[v]; !ok {
			t.Errorf("expected version %s in merged packument", v)
		}
	}
	v110, _ := versions["1.1.0"].(map[string]any)
	dist, _ := v110["dist"].(map[string]any)
	tarball, _ := dist["tarball"].(string)
	if !strings.Contains(tarball, "/npm-group/") {
		t.Errorf("tarball URL should point to group repo, got %q", tarball)
	}
	dt, _ := result["dist-tags"].(map[string]any)
	if dt["latest"] != "1.1.0" {
		t.Errorf("expected dist-tag latest=1.1.0 (first member wins), got %v", dt["latest"])
	}
}

func TestGroup_PackumentNotFound(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "npm-a", Format: "npm", Kind: repo.Hosted},
		{Name: "npm-group", Format: "npm", Kind: repo.Group, Members: []string{"npm-a"}},
	} {
		mgr.Add(r)
	}
	groupRepo, _ := mgr.Get("npm-group")
	c := &format.Context{Repo: groupRepo, Blob: b, Meta: m, Repos: mgr}

	req := httptest.NewRequest(http.MethodGet, "/repository/npm-group/nosuchpkg", nil)
	rw := httptest.NewRecorder()
	New().groupPackument(rw, req, c, "nosuchpkg")

	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

// --- packument & tarball (GET paths not exercised by the existing tests) ----

func TestServe_GetPackument(t *testing.T) {
	c, _, m := hostedCtx(t)
	seedPackument(t, m, "npm-hosted", "mylib", []string{"1.0.0"}, map[string]string{"latest": "1.0.0"})

	c.Sub = "mylib"
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(http.MethodGet, "/", nil), c)
	if rw.Code != http.StatusOK {
		t.Fatalf("GET packument: %d\n%s", rw.Code, rw.Body)
	}
	var doc map[string]any
	json.NewDecoder(rw.Body).Decode(&doc)
	if doc["name"] != "mylib" {
		t.Fatalf("name mismatch: %v", doc["name"])
	}
}

func TestServe_GetPackument_NotFound(t *testing.T) {
	c, _, _ := hostedCtx(t)
	c.Sub = "ghost-pkg"
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(http.MethodGet, "/", nil), c)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

func TestServe_GetTarball(t *testing.T) {
	c, b, _ := hostedCtx(t)
	b.Put("npm-hosted/mylib/-/mylib-1.0.0.tgz", strings.NewReader("tarball-bytes"))

	c.Sub = "mylib/-/mylib-1.0.0.tgz"
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(http.MethodGet, "/", nil), c)
	if rw.Code != http.StatusOK {
		t.Fatalf("GET tarball: %d", rw.Code)
	}
	if rw.Body.String() != "tarball-bytes" {
		t.Fatalf("body mismatch: %q", rw.Body)
	}
}

func TestServe_GetTarball_NotFound(t *testing.T) {
	c, _, _ := hostedCtx(t)
	c.Sub = "mylib/-/mylib-9.9.9.tgz"
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(http.MethodGet, "/", nil), c)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

func TestBrowseRepo_NPM(t *testing.T) {
	c, _, m := hostedCtx(t)
	seedPackument(t, m, "npm-hosted", "alpha", []string{"1.0.0", "2.0.0"}, map[string]string{"latest": "2.0.0"})
	seedPackument(t, m, "npm-hosted", "beta", []string{"0.1.0"}, map[string]string{"latest": "0.1.0"})

	entries, err := New().BrowseRepo(c)
	if err != nil {
		t.Fatalf("BrowseRepo: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(entries), entries)
	}
	if entries[0].Name != "alpha" {
		t.Fatalf("expected alpha first, got %v", entries[0])
	}
	if len(entries[0].Versions) != 2 {
		t.Fatalf("expected 2 versions for alpha, got %v", entries[0].Versions)
	}
}

func TestFormat_NPM(t *testing.T) {
	if got := New().Format(); got != "npm" {
		t.Fatalf("Format() = %q, want npm", got)
	}
}

// TestServe_ProxyPackument exercises proxyNS + proxyPackument via a mock upstream.
func TestServe_ProxyPackument(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"name": "upstream-pkg",
			"versions": map[string]any{
				"1.0.0": map[string]any{
					"name": "upstream-pkg", "version": "1.0.0",
					"dist": map[string]any{
						"tarball": "https://upstream.example.com/upstream-pkg/-/upstream-pkg-1.0.0.tgz",
					},
				},
			},
		})
	}))
	defer upstream.Close()

	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{
			Name: "npm-proxy", Format: "npm", Kind: repo.Proxy,
			Upstream: upstream.URL,
		},
		Blob: b, Meta: m,
		HTTP: upstream.Client(),
	}

	c.Sub = "upstream-pkg"
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(http.MethodGet, "/", nil), c)
	if rw.Code != http.StatusOK {
		t.Fatalf("proxy packument: got %d\n%s", rw.Code, rw.Body)
	}
	var doc map[string]any
	json.NewDecoder(rw.Body).Decode(&doc)
	if doc["name"] != "upstream-pkg" {
		t.Fatalf("name mismatch: %v", doc["name"])
	}
}

// TestServe_GroupTarball exercises the groupTarball code path.
func TestServe_GroupTarball(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))

	// Seed a tarball in the member repo's namespace.
	b.Put("npm-member/mylib/-/mylib-1.0.0.tgz", strings.NewReader("group-tarball-bytes"))

	mgr := repo.NewManager()
	mgr.Add(repo.Repository{Name: "npm-member", Format: "npm", Kind: repo.Hosted})
	mgr.Add(repo.Repository{
		Name: "npm-group", Format: "npm", Kind: repo.Group,
		Members: []string{"npm-member"},
	})

	groupRepo, _ := mgr.Get("npm-group")
	c := &format.Context{
		Repo: groupRepo, Blob: b, Meta: m, Repos: mgr,
	}

	c.Sub = "mylib/-/mylib-1.0.0.tgz"
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(http.MethodGet, "/", nil), c)
	if rw.Code != http.StatusOK {
		t.Fatalf("group tarball: got %d", rw.Code)
	}
	if rw.Body.String() != "group-tarball-bytes" {
		t.Fatalf("body mismatch: %q", rw.Body)
	}
}

// TestServe_GroupTarball_NotFound verifies the group 404 fallback.
func TestServe_GroupTarball_NotFound(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))

	mgr := repo.NewManager()
	mgr.Add(repo.Repository{Name: "npm-member", Format: "npm", Kind: repo.Hosted})
	mgr.Add(repo.Repository{Name: "npm-group", Format: "npm", Kind: repo.Group, Members: []string{"npm-member"}})

	groupRepo, _ := mgr.Get("npm-group")
	c := &format.Context{Repo: groupRepo, Blob: b, Meta: m, Repos: mgr}

	c.Sub = "ghost/-/ghost-9.9.9.tgz"
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(http.MethodGet, "/", nil), c)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

// ── Inspect ───────────────────────────────────────────────────────────────────

func TestInspect_Hosted(t *testing.T) {
	c, _, m := hostedCtx(t)
	seedPackument(t, m, "npm-hosted", "mylib", []string{"1.0.0", "2.0.0"},
		map[string]string{"latest": "2.0.0"})
	// Seed a per-version record with a dependency so deps parsing is exercised.
	m.PutJSON("npm-hosted:npm:v", "mylib:2.0.0", map[string]any{ //nolint:errcheck
		"name": "mylib", "version": "2.0.0",
		"dependencies": map[string]any{"lodash": "^4.17.0"},
	})

	detail, ok := New().Inspect(c, "http://localhost:8080", "mylib")
	if !ok {
		t.Fatal("expected Inspect to succeed")
	}
	if detail.Name != "mylib" {
		t.Errorf("name: got %q", detail.Name)
	}
	if len(detail.Versions) != 2 {
		t.Errorf("versions: got %d, want 2", len(detail.Versions))
	}
	if len(detail.Deps) != 1 || detail.Deps[0].Name != "lodash" {
		t.Errorf("deps: %+v", detail.Deps)
	}
	if !strings.Contains(detail.InstallSnippet, "mylib") {
		t.Errorf("snippet: %s", detail.InstallSnippet)
	}
}

func TestInspect_NoPackument(t *testing.T) {
	c, _, _ := hostedCtx(t)
	if _, ok := New().Inspect(c, "http://localhost:8080", "ghost"); ok {
		t.Fatal("expected false for missing packument")
	}
}

func TestInspect_EmptyVersions(t *testing.T) {
	c, _, m := hostedCtx(t)
	// Packument with no versions field.
	m.PutJSON("npm-hosted:npm", "empty-pkg", map[string]any{"name": "empty-pkg"}) //nolint:errcheck
	if _, ok := New().Inspect(c, "http://localhost:8080", "empty-pkg"); ok {
		t.Fatal("expected false when packument has no versions")
	}
}

func TestInspect_Group(t *testing.T) {
	b, m := func() (blob.Store, meta.Store) {
		dir := t.TempDir()
		b, _ := blob.NewFS(filepath.Join(dir, "b"))
		m, _ := meta.NewFS(filepath.Join(dir, "m"))
		return b, m
	}()
	seedPackument(t, m, "npm-member", "mylib", []string{"1.0.0"},
		map[string]string{"latest": "1.0.0"})

	mgr := repo.NewManager()
	mgr.Add(repo.Repository{Name: "npm-member", Format: "npm", Kind: repo.Hosted}) //nolint:errcheck
	mgr.Add(repo.Repository{Name: "npm-group", Format: "npm", Kind: repo.Group, Members: []string{"npm-member"}}) //nolint:errcheck

	groupRepo, _ := mgr.Get("npm-group")
	c := &format.Context{Repo: groupRepo, Blob: b, Meta: m, Repos: mgr}

	detail, ok := New().Inspect(c, "http://localhost:8080", "mylib")
	if !ok {
		t.Fatal("expected group Inspect to succeed")
	}
	if detail.Name != "mylib" {
		t.Errorf("name: got %q", detail.Name)
	}
}

// ── groupTarball ──────────────────────────────────────────────────────────────

func TestGroupTarball_FromHostedMember(t *testing.T) {
	b, m := func() (blob.Store, meta.Store) {
		dir := t.TempDir()
		b, _ := blob.NewFS(filepath.Join(dir, "b"))
		m, _ := meta.NewFS(filepath.Join(dir, "m"))
		return b, m
	}()
	b.Put("npm-member/mylib/-/mylib-1.0.0.tgz", strings.NewReader("tarbytes")) //nolint:errcheck

	mgr := repo.NewManager()
	mgr.Add(repo.Repository{Name: "npm-member", Format: "npm", Kind: repo.Hosted}) //nolint:errcheck
	mgr.Add(repo.Repository{Name: "npm-group", Format: "npm", Kind: repo.Group, Members: []string{"npm-member"}}) //nolint:errcheck

	groupRepo, _ := mgr.Get("npm-group")
	c := &format.Context{Repo: groupRepo, Blob: b, Meta: m, Repos: mgr}

	c.Sub = "mylib/-/mylib-1.0.0.tgz"
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(http.MethodGet, "/", nil), c)
	if rw.Code != http.StatusOK {
		t.Fatalf("group tarball from hosted member: got %d", rw.Code)
	}
	if rw.Body.String() != "tarbytes" {
		t.Errorf("body: got %q", rw.Body.String())
	}
}

func TestGroupTarball_NotFound(t *testing.T) {
	b, m := func() (blob.Store, meta.Store) {
		dir := t.TempDir()
		b, _ := blob.NewFS(filepath.Join(dir, "b"))
		m, _ := meta.NewFS(filepath.Join(dir, "m"))
		return b, m
	}()
	mgr := repo.NewManager()
	mgr.Add(repo.Repository{Name: "npm-member", Format: "npm", Kind: repo.Hosted}) //nolint:errcheck
	mgr.Add(repo.Repository{Name: "npm-group", Format: "npm", Kind: repo.Group, Members: []string{"npm-member"}}) //nolint:errcheck

	groupRepo, _ := mgr.Get("npm-group")
	c := &format.Context{Repo: groupRepo, Blob: b, Meta: m, Repos: mgr}

	c.Sub = "ghost/-/ghost-9.9.9.tgz"
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(http.MethodGet, "/", nil), c)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

// ── publish error paths ───────────────────────────────────────────────────────

func TestPublish_BadJSON(t *testing.T) {
	c, _, _ := hostedCtx(t)
	c.Sub = "mylib"
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader("not json"))
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad JSON, got %d", rw.Code)
	}
}

func TestPublish_BadAttachmentEncoding(t *testing.T) {
	c, _, _ := hostedCtx(t)
	c.Sub = "mylib"
	payload := `{"name":"mylib","versions":{},"_attachments":{"mylib-1.0.0.tgz":{"data":"!!!not-base64!!!"}}}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(payload))
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad attachment, got %d", rw.Code)
	}
}
