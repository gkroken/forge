package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

	mgr.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted})    //nolint:errcheck
	mgr.Add(repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted})  //nolint:errcheck

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
