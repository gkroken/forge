package npm

import (
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

func TestGroup_PackumentMerge(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))

	h := New()

	// Seed packuments in two hosted member repos.
	seedPackument := func(repoName, pkg string, versions []string, latestTag string) {
		ns := repoName + ":npm"
		vers := map[string]any{}
		for _, v := range versions {
			vers[v] = map[string]any{
				"name":    pkg,
				"version": v,
				"dist": map[string]any{
					"tarball": "http://registry.npmjs.org/" + pkg + "/-/" + pkg + "-" + v + ".tgz",
				},
			}
		}
		doc := map[string]any{
			"name":      pkg,
			"versions":  vers,
			"dist-tags": map[string]any{"latest": latestTag},
		}
		if err := m.PutJSON(ns, pkg, doc); err != nil {
			t.Fatal(err)
		}
	}

	seedPackument("npm-a", "mylib", []string{"1.0.0", "1.1.0"}, "1.1.0")
	seedPackument("npm-b", "mylib", []string{"1.1.0", "2.0.0"}, "2.0.0") // 1.1.0 overlaps
	seedPackument("npm-b", "otherlib", []string{"3.0.0"}, "3.0.0")

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
	c := &format.Context{
		Repo:  groupRepo,
		Blob:  b,
		Meta:  m,
		Repos: mgr,
	}

	req := httptest.NewRequest(http.MethodGet, "/repository/npm-group/mylib", nil)
	req.Host = "localhost:8080"
	rw := httptest.NewRecorder()
	h.groupPackument(rw, req, c, "mylib")

	if rw.Code != http.StatusOK && rw.Code != 0 {
		t.Fatalf("unexpected status %d", rw.Code)
	}
	var result map[string]any
	if err := json.NewDecoder(rw.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}

	versions, _ := result["versions"].(map[string]any)
	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		if _, ok := versions[v]; !ok {
			t.Errorf("expected version %s in merged packument", v)
		}
	}
	// 1.1.0 should appear exactly once (deduplicated).
	if len(versions) != 3 {
		t.Errorf("expected 3 versions, got %d", len(versions))
	}

	// Tarball URLs should point to the group repo, not member repos.
	v110, _ := versions["1.1.0"].(map[string]any)
	dist, _ := v110["dist"].(map[string]any)
	tarball, _ := dist["tarball"].(string)
	if !strings.Contains(tarball, "/npm-group/") {
		t.Errorf("tarball URL should point to group repo, got %q", tarball)
	}

	// dist-tags: npm-a's latest (1.1.0) wins since npm-a is first member.
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
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
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
