package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/helm"
	"forge/internal/format/npm"
	"forge/internal/meta"
	"forge/internal/repo"
)

// newBrowseServer wires a Server with npm and helm handlers and seeds one
// hosted repo of each format with a small amount of data.
func newBrowseServer(t *testing.T) (*Server, *format.Registry) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()

	reg := format.NewRegistry()
	reg.Register(npm.New())
	reg.Register(helm.New())

	mgr.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted})   //nolint:errcheck
	mgr.Add(repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted}) //nolint:errcheck

	// Seed one npm packument so BrowseRepo returns something.
	m.PutJSON("npm-hosted:npm", "lodash", map[string]any{ //nolint:errcheck
		"name":     "lodash",
		"versions": map[string]any{"4.17.21": map[string]any{}, "4.17.20": map[string]any{}},
	})

	// Seed two helm chart records.
	m.PutJSON("helm-hosted:helm", "mychart-1.0.0", map[string]any{ //nolint:errcheck
		"name": "mychart", "version": "1.0.0", "digest": "abc", "created": "2024-01-01", "filename": "mychart-1.0.0.tgz",
	})
	m.PutJSON("helm-hosted:helm", "mychart-1.1.0", map[string]any{ //nolint:errcheck
		"name": "mychart", "version": "1.1.0", "digest": "def", "created": "2024-02-01", "filename": "mychart-1.1.0.tgz",
	})

	return New(mgr, reg, b, m, nil), reg
}

func TestComponents_npm(t *testing.T) {
	srv, _ := newBrowseServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-hosted/components", nil))

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
	}
	var resp componentsResponse
	if err := json.NewDecoder(rw.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 {
		t.Errorf("total: got %d, want 1", resp.Total)
	}
	if len(resp.Components) != 1 || resp.Components[0].Name != "lodash" {
		t.Errorf("unexpected components: %+v", resp.Components)
	}
	if len(resp.Components[0].Versions) != 2 {
		t.Errorf("expected 2 versions, got %v", resp.Components[0].Versions)
	}
}

func TestComponents_helm(t *testing.T) {
	srv, _ := newBrowseServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/helm-hosted/components", nil))

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	var resp componentsResponse
	json.NewDecoder(rw.Body).Decode(&resp)
	if resp.Total != 1 {
		t.Errorf("total: got %d, want 1", resp.Total)
	}
	if resp.Components[0].Name != "mychart" {
		t.Errorf("unexpected name: %s", resp.Components[0].Name)
	}
	if len(resp.Components[0].Versions) != 2 {
		t.Errorf("expected 2 versions, got %v", resp.Components[0].Versions)
	}
}

func TestComponents_filter(t *testing.T) {
	srv, _ := newBrowseServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-hosted/components?q=dash", nil))

	var resp componentsResponse
	json.NewDecoder(rw.Body).Decode(&resp)
	if resp.Total != 1 || resp.Components[0].Name != "lodash" {
		t.Errorf("filter failed: %+v", resp)
	}
}

func TestComponents_filterNoMatch(t *testing.T) {
	srv, _ := newBrowseServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-hosted/components?q=zzz", nil))

	var resp componentsResponse
	json.NewDecoder(rw.Body).Decode(&resp)
	if resp.Total != 0 || len(resp.Components) != 0 {
		t.Errorf("expected empty, got %+v", resp)
	}
}

func TestComponents_notFound(t *testing.T) {
	srv, _ := newBrowseServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/no-such-repo/components", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

func TestSearch_basic(t *testing.T) {
	srv, _ := newBrowseServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=lodash", nil))

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	var resp searchResponse
	json.NewDecoder(rw.Body).Decode(&resp)
	if len(resp.Results) != 1 {
		t.Errorf("expected 1 result, got %d", len(resp.Results))
	}
	if resp.Results[0].Name != "lodash" || resp.Results[0].Repo != "npm-hosted" {
		t.Errorf("unexpected result: %+v", resp.Results[0])
	}
}

func TestSearch_formatFilter(t *testing.T) {
	srv, _ := newBrowseServer(t)
	rw := httptest.NewRecorder()
	// "my" matches "mychart" in helm but nothing in npm
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=my&format=helm", nil))

	var resp searchResponse
	json.NewDecoder(rw.Body).Decode(&resp)
	if len(resp.Results) != 1 || resp.Results[0].Format != "helm" {
		t.Errorf("expected 1 helm result, got %+v", resp.Results)
	}
}

// TestComponents_unboundedLimit verifies limit=0 returns the full set, past the
// old 200 cap, while a positive limit still paginates.
func TestComponents_unboundedLimit(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	reg.Register(npm.New())
	mgr.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted}) //nolint:errcheck

	const n = 250
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("pkg-%03d", i)
		m.PutJSON("npm-hosted:npm", name, map[string]any{ //nolint:errcheck
			"name": name, "versions": map[string]any{"1.0.0": map[string]any{}},
		})
	}
	srv := New(mgr, reg, b, m, nil)

	get := func(query string) componentsResponse {
		rw := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-hosted/components"+query, nil))
		if rw.Code != http.StatusOK {
			t.Fatalf("%q: status %d", query, rw.Code)
		}
		var resp componentsResponse
		if err := json.NewDecoder(rw.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	all := get("?limit=0")
	if all.Total != n || len(all.Components) != n {
		t.Errorf("limit=0: got total=%d len=%d, want %d", all.Total, len(all.Components), n)
	}
	if alias := get("?limit=all"); len(alias.Components) != n {
		t.Errorf("limit=all: got %d, want %d", len(alias.Components), n)
	}
	if paged := get("?limit=10&page=2"); paged.Total != n || len(paged.Components) != 10 {
		t.Errorf("limit=10&page=2: got total=%d len=%d, want total=%d len=10", paged.Total, len(paged.Components), n)
	}
}

// newMavenTreeServer wires a server with one maven repo seeded with a couple of
// artifacts laid out in standard Maven 2 layout, so the tree endpoint has a
// real groupId → artifactId → version → file hierarchy to classify.
func newMavenTreeServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	mgr.Add(repo.Repository{Name: "maven-releases", Format: "maven", Kind: repo.Hosted}) //nolint:errcheck

	keys := []string{
		"maven-releases/org/springframework/spring-core/6.2.7/spring-core-6.2.7.jar",
		"maven-releases/org/springframework/spring-core/6.2.6/spring-core-6.2.6.jar",
		"maven-releases/org/springframework/spring-core/maven-metadata.xml",
		"maven-releases/org/springframework/spring-beans/6.2.7/spring-beans-6.2.7.jar",
	}
	for _, k := range keys {
		b.Put(k, strings.NewReader("x")) //nolint:errcheck
	}
	return New(mgr, reg, b, m, nil)
}

func fetchTree(t *testing.T, srv *Server, prefix string) []treeNode {
	t.Helper()
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/ui/browse/maven-releases/tree?prefix="+prefix, nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("tree(%q): status %d: %s", prefix, rw.Code, rw.Body.String())
	}
	var nodes []treeNode
	if err := json.NewDecoder(rw.Body).Decode(&nodes); err != nil {
		t.Fatal(err)
	}
	return nodes
}

// TestBrowseTree_terminatesAtArtifact verifies the tree descends through groupId
// segments as folders but stops at the artifact (the directory whose children are
// versions), emitting a Component identifier instead of recursing into versions.
func TestBrowseTree_terminatesAtArtifact(t *testing.T) {
	srv := newMavenTreeServer(t)

	// Top level: "org" is a plain groupId folder, not an artifact.
	top := fetchTree(t, srv, "")
	if len(top) != 1 || top[0].Name != "org" {
		t.Fatalf("root tree: got %+v, want single folder 'org'", top)
	}
	if !top[0].IsDir || top[0].Component != "" {
		t.Errorf("'org' should be a folder, got %+v", top[0])
	}

	// org/springframework: both artifacts terminate here (their children are
	// versions), so each is a Component leaf, not a descendable folder.
	lvl := fetchTree(t, srv, "org/springframework")
	byName := map[string]treeNode{}
	for _, n := range lvl {
		byName[n.Name] = n
	}
	if len(byName) != 2 {
		t.Fatalf("expected spring-core + spring-beans, got %+v", lvl)
	}
	core, ok := byName["spring-core"]
	if !ok {
		t.Fatalf("missing spring-core in %+v", lvl)
	}
	if core.IsDir {
		t.Errorf("spring-core should not be a descendable folder: %+v", core)
	}
	if core.Component != "org.springframework:spring-core" {
		t.Errorf("spring-core component: got %q, want org.springframework:spring-core", core.Component)
	}
	if beans := byName["spring-beans"]; beans.Component != "org.springframework:spring-beans" {
		t.Errorf("spring-beans component: got %q", beans.Component)
	}
}

func TestSearch_emptyQuery(t *testing.T) {
	srv, _ := newBrowseServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/search", nil))

	var resp searchResponse
	json.NewDecoder(rw.Body).Decode(&resp)
	if len(resp.Results) != 0 {
		t.Errorf("expected empty results for empty query, got %v", resp.Results)
	}
}

func TestClampedInt(t *testing.T) {
	cases := []struct {
		query string
		want  int
	}{
		{"", 10},     // absent → default
		{"abc", 10},  // invalid → default
		{"0", 1},     // below min → min
		{"999", 100}, // above max → max
		{"50", 50},   // in range → value
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/?n="+tc.query, nil)
		got := clampedInt(r, "n", 10, 1, 100)
		if got != tc.want {
			t.Errorf("clampedInt(%q): got %d, want %d", tc.query, got, tc.want)
		}
	}
}
