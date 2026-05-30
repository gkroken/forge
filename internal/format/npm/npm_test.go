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
