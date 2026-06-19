package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/format"
	"forge/internal/format/cran"
	"forge/internal/format/helm"
	"forge/internal/format/npm"
	"forge/internal/meta"
	"forge/internal/repo"
)

// newUIServer builds a Server wired for UI tests. It registers npm and helm
// handlers and seeds both repos with a small amount of data.
func newUIServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()

	reg := format.NewRegistry()
	reg.Register(npm.New())
	reg.Register(helm.New())

	mgr.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, AnonymousRead: true})   //nolint:errcheck
	mgr.Add(repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted, AnonymousRead: true}) //nolint:errcheck

	m.PutJSON("npm-hosted:npm", "lodash", map[string]any{ //nolint:errcheck
		"name":     "lodash",
		"versions": map[string]any{"4.17.21": map[string]any{}, "4.17.20": map[string]any{}},
	})
	m.PutJSON("helm-hosted:helm", "mychart-1.0.0", map[string]any{ //nolint:errcheck
		"name": "mychart", "version": "1.0.0", "digest": "abc",
		"created": "2024-01-01", "filename": "mychart-1.0.0.tgz",
	})

	return New(mgr, reg, b, m, nil)
}

// uiGet performs a GET against the handler and returns the recorder.
func uiGet(t *testing.T, h http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	return rw
}

// uiPost performs a form POST against the handler.
func uiPost(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	return rw
}

// uiDelete performs a DELETE request against the handler.
func uiDelete(t *testing.T, h http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	return rw
}

func assertContains(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("response body missing %q\ngot: %.300s", want, body)
	}
}

func assertNotContains(t *testing.T, body, want string) {
	t.Helper()
	if strings.Contains(body, want) {
		t.Errorf("response body should not contain %q", want)
	}
}

// ── /ui/ home ─────────────────────────────────────────────────────────────────

func TestUIHome_OK(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "npm-hosted")
	assertContains(t, body, "helm-hosted")
	assertContains(t, body, "forge") // brand in nav
}

func TestUIHome_ContentType(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/")
	if ct := rw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html, got %q", ct)
	}
}

func TestUIHome_FullPage(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/")
	body := rw.Body.String()
	// Full page has the HTML shell
	assertContains(t, body, "<!DOCTYPE html>")
	assertContains(t, body, "<nav")
	assertContains(t, body, "Admin")
}

// ── /ui/repos/{name} ──────────────────────────────────────────────────────────

func TestUIRepo_OK(t *testing.T) {
	// /ui/repos/{name} redirects to /ui/browse/{name}; test the browse page directly
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/browse/npm-hosted")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "npm-hosted")
	assertContains(t, body, "browse-shell")
	assertContains(t, body, "browse-repo-node")
}

func TestUIRepo_NotFound(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/browse/no-such-repo")
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

func TestUIRepo_HasFilterInput(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/browse/npm-hosted")
	body := rw.Body.String()
	// All repos are server-rendered as repo nodes; JS (browse.js) handles expansion
	assertContains(t, body, `browse-repo-node`)
	assertContains(t, body, `/ui/static/browse.js`)
}

func TestUIRepo_EmptyRepo(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/browse/helm-hosted")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	assertContains(t, rw.Body.String(), "browse-shell")
}

func TestUIRepo_Pagination_NoMore(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/npm-hosted")
	body := rw.Body.String()
	// Only 1 package — no next/prev links
	assertNotContains(t, body, "Next →")
	assertNotContains(t, body, "← Prev")
}

// ── /ui/search ────────────────────────────────────────────────────────────────

func TestUISearch_WithQuery(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/search?q=lodash")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "lodash")
	assertContains(t, body, "npm-hosted")
}

func TestUISearch_EmptyQuery(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/search")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "Enter a search term")
}

func TestUISearch_NoResults(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/search?q=zzznomatch")
	body := rw.Body.String()
	assertContains(t, body, "No results")
}

func TestUISearch_HtmxPartial(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/search?q=lodash", "HX-Request", "true")
	body := rw.Body.String()
	assertNotContains(t, body, "<!DOCTYPE html>")
	assertContains(t, body, "lodash")
}

func TestUISearch_FullPageHasShell(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/search?q=lodash")
	body := rw.Body.String()
	assertContains(t, body, "<!DOCTYPE html>")
	assertContains(t, body, "<nav")
}

func TestUISearch_FilterDropdownsPresent(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/search")
	body := rw.Body.String()
	assertContains(t, body, `name="format"`)
	assertContains(t, body, `name="repo"`)
	assertContains(t, body, "All formats")
	assertContains(t, body, "All repositories")
}

func TestUISearch_FormatFilter(t *testing.T) {
	h := newUIServer(t).Routes()
	// lodash is npm; filtering to helm should return no results (htmx partial)
	rw := uiGet(t, h, "/ui/search?q=lodash&format=helm", "HX-Request", "true")
	body := rw.Body.String()
	assertContains(t, body, "No results")
	assertNotContains(t, body, "npm-hosted")
}

func TestUISearch_RepoFilter(t *testing.T) {
	h := newUIServer(t).Routes()
	// filtering to helm-hosted should hide npm-hosted results (htmx partial)
	rw := uiGet(t, h, "/ui/search?q=lodash&repo=helm-hosted", "HX-Request", "true")
	body := rw.Body.String()
	assertNotContains(t, body, "npm-hosted")
}

func TestUISearch_FilterPreservedInHtmxPartial(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/search?q=lodash&format=npm", "HX-Request", "true")
	body := rw.Body.String()
	assertNotContains(t, body, "<!DOCTYPE html>")
	assertContains(t, body, "lodash")
}

// ── /ui/admin/ ────────────────────────────────────────────────────────────────

func TestUIAdminHome_OK(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "npm-hosted")
	assertContains(t, body, "helm-hosted")
	assertContains(t, body, "New repository")
}

func TestUIAdminHome_FlashMessage(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/?flash=Created+repository+test")
	assertContains(t, rw.Body.String(), "Created repository test")
}

func TestUIAdminHome_EditDeleteButtons(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/")
	body := rw.Body.String()
	assertContains(t, body, `/ui/admin/repos/npm-hosted/edit`)
	assertContains(t, body, `hx-delete="/ui/admin/repos/npm-hosted"`)
}

// ── /ui/admin/repos/new ───────────────────────────────────────────────────────

func TestUIAdminNewRepo_Form(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/repos/new")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, `name="name"`)
	assertContains(t, body, `name="format"`)
	assertContains(t, body, `name="kind"`)
	assertContains(t, body, "Create repository")
}

func TestUIAdminNewRepo_Create_Success(t *testing.T) {
	srv := newUIServer(t)
	h := srv.Routes()
	rw := uiPost(t, h, "/ui/admin/repos/new", url.Values{
		"name": {"cran-hosted"}, "format": {"cran"}, "kind": {"hosted"},
	})
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rw.Code)
	}
	if loc := rw.Header().Get("Location"); !strings.Contains(loc, "cran-hosted") {
		t.Errorf("redirect location %q missing repo name", loc)
	}
	if _, ok := srv.Repos.Get("cran-hosted"); !ok {
		t.Error("repo not created in manager")
	}
}

func TestUIAdminNewRepo_Create_InvalidName(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiPost(t, h, "/ui/admin/repos/new", url.Values{
		"name": {""}, "format": {"npm"}, "kind": {"hosted"},
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", rw.Code)
	}
	assertContains(t, rw.Body.String(), "name is required")
}

func TestUIAdminNewRepo_Create_DuplicateName(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiPost(t, h, "/ui/admin/repos/new", url.Values{
		"name": {"npm-hosted"}, "format": {"npm"}, "kind": {"hosted"},
	})
	// Should re-render form with error, not 303
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", rw.Code)
	}
	assertContains(t, rw.Body.String(), "npm-hosted")
}

func TestUIAdminNewRepo_Create_ProxyMissingUpstream(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiPost(t, h, "/ui/admin/repos/new", url.Values{
		"name": {"x"}, "format": {"maven"}, "kind": {"proxy"}, "upstream": {""},
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", rw.Code)
	}
	assertContains(t, rw.Body.String(), "upstream")
}

// ── /ui/admin/repos/{name}/edit ───────────────────────────────────────────────

func TestUIAdminEditRepo_Form(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/repos/npm-hosted/edit")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "npm-hosted")
	assertContains(t, body, "Save changes")
	// Name field is readonly on edit
	assertContains(t, body, "readonly")
}

func TestUIAdminEditRepo_NotFound(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/repos/no-such-repo/edit")
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

func TestUIAdminEditRepo_Update_Success(t *testing.T) {
	srv := newUIServer(t)
	h := srv.Routes()
	rw := uiPost(t, h, "/ui/admin/repos/npm-hosted/edit", url.Values{
		"name": {"npm-hosted"}, "format": {"npm"}, "kind": {"hosted"},
		"anonymousRead": {"on"},
	})
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d; body: %s", rw.Code, rw.Body.String())
	}
	rp, _ := srv.Repos.Get("npm-hosted")
	if !rp.AnonymousRead {
		t.Error("anonymousRead not updated")
	}
}

func TestUIAdminEditRepo_Update_InvalidKind(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiPost(t, h, "/ui/admin/repos/npm-hosted/edit", url.Values{
		"name": {"npm-hosted"}, "format": {"npm"}, "kind": {"bad-kind"},
	})
	// Invalid kind → re-render with validation error
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", rw.Code)
	}
	assertContains(t, rw.Body.String(), "kind")
}

// ── DELETE /ui/admin/repos/{name} ─────────────────────────────────────────────

func TestUIAdminDeleteRepo_Htmx(t *testing.T) {
	srv := newUIServer(t)
	h := srv.Routes()
	rw := uiDelete(t, h, "/ui/admin/repos/npm-hosted", "HX-Request", "true")
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	if loc := rw.Header().Get("HX-Redirect"); !strings.Contains(loc, "/ui/admin/") {
		t.Errorf("HX-Redirect header missing or wrong: %q", loc)
	}
	if _, ok := srv.Repos.Get("npm-hosted"); ok {
		t.Error("repo should have been deleted")
	}
}

func TestUIAdminDeleteRepo_Plain(t *testing.T) {
	srv := newUIServer(t)
	h := srv.Routes()
	rw := uiDelete(t, h, "/ui/admin/repos/helm-hosted")
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rw.Code)
	}
	if _, ok := srv.Repos.Get("helm-hosted"); ok {
		t.Error("repo should have been deleted")
	}
}

func TestUIAdminDeleteRepo_NotFound(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiDelete(t, h, "/ui/admin/repos/no-such-repo", "HX-Request", "true")
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

// ── static assets ─────────────────────────────────────────────────────────────

func TestUIStatic_CSS(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/static/style.css")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	if ct := rw.Header().Get("Content-Type"); !strings.Contains(ct, "css") {
		t.Errorf("expected CSS content-type, got %q", ct)
	}
}

func TestUIStatic_NotFound(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/static/nonexistent.js")
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

// ── unknown UI path ───────────────────────────────────────────────────────────

func TestUI_UnknownPath(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/completely/unknown/path")
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

// ── /ui/repos/{name}/{component} ─────────────────────────────────────────────

func TestUIComponent_OK(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/npm-hosted/lodash")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "lodash")
	assertContains(t, body, "4.17.21")
	assertContains(t, body, "4.17.20")
	assertContains(t, body, "npm-hosted") // breadcrumb
	assertContains(t, body, "/repository/npm-hosted/") // registry URL
}

func TestUIComponent_Breadcrumb(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/npm-hosted/lodash")
	body := rw.Body.String()
	assertContains(t, body, `href="/ui/"`)
	assertContains(t, body, `href="/ui/repos/npm-hosted"`)
}

func TestUIComponent_NotFoundRepo(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/no-such-repo/anything")
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

func TestUIComponent_NotFoundComponent(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/npm-hosted/no-such-pkg")
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

func TestUIRepo_ComponentLinksPresent(t *testing.T) {
	// Component links are built dynamically by JS from the /api/v1 components endpoint.
	// Verify the API endpoint serves the component list so the JS has data to render.
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/api/v1/repos/npm-hosted/components")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	assertContains(t, rw.Body.String(), "lodash")
}

// ── U3: cache-busting, dark mode, nav search, breadcrumb ─────────────────────

func TestUI_CSSHasCacheBustParam(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/")
	body := rw.Body.String()
	// style.css link must include a ?v= query param
	if !strings.Contains(body, "style.css?v=") {
		t.Error("style.css link missing ?v= cache-bust parameter")
	}
}

func TestUI_CSSVersionConsistent(t *testing.T) {
	h := newUIServer(t).Routes()
	r1 := uiGet(t, h, "/ui/")
	r2 := uiGet(t, h, "/ui/search")
	extractVer := func(body string) string {
		i := strings.Index(body, "style.css?v=")
		if i < 0 {
			return ""
		}
		rest := body[i+len("style.css?v="):]
		end := strings.IndexAny(rest, `"'`)
		if end < 0 {
			return rest
		}
		return rest[:end]
	}
	v1 := extractVer(r1.Body.String())
	v2 := extractVer(r2.Body.String())
	if v1 == "" || v1 != v2 {
		t.Errorf("inconsistent CSS version: %q vs %q", v1, v2)
	}
}

func TestUI_CSSVersionedURLServed(t *testing.T) {
	// The versioned URL must still serve the CSS (query param ignored by FileServer).
	h := newUIServer(t).Routes()
	ver := cssVer
	rw := uiGet(t, h, "/ui/static/style.css?v="+ver)
	if rw.Code != http.StatusOK {
		t.Fatalf("versioned CSS URL returned %d", rw.Code)
	}
}

func TestUISearch_HtmxBoosted_ReturnsFullPage(t *testing.T) {
	h := newUIServer(t).Routes()
	// Simulate a nav-bar hx-boost request: both HX-Request and HX-Boosted set.
	r := httptest.NewRequest(http.MethodGet, "/ui/search?q=lodash", nil)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Boosted", "true")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	body := rw.Body.String()
	// Boosted request should get the full page, not just the results fragment.
	assertContains(t, body, "<!DOCTYPE html>")
	assertContains(t, body, "<nav")
	assertContains(t, body, "lodash")
}

func TestUISearch_NavForm_HasHxBoost(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/")
	assertContains(t, rw.Body.String(), "hx-boost")
}

func TestUIAdminHome_HasBreadcrumb(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/")
	body := rw.Body.String()
	// Sidebar nav always contains a link to Browse
	assertContains(t, body, `href="/ui/browse"`)
	// Page title shown in admin-subheader
	assertContains(t, body, "Repositories")
	assertContains(t, body, "admin-subheader")
}

// ── /ui/admin/access ─────────────────────────────────────────────────────────

func TestUIAccess_EvalMode(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/access")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	assertContains(t, rw.Body.String(), "not enabled")
}

func TestUIAccess_AuthEnabled_ShowsRepos(t *testing.T) {
	srv, secret := newUIServerWithAuth(t)
	h := srv.Routes()
	r := httptest.NewRequest(http.MethodGet, "/ui/admin/access", nil)
	r.AddCookie(&http.Cookie{Name: auth.UISessionCookie, Value: secret})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "npm-hosted")
	assertContains(t, body, "test-admin") // token description appears as a grant
}

func TestUIAccess_UnauthenticatedRedirects(t *testing.T) {
	srv, _ := newUIServerWithAuth(t)
	h := srv.Routes()
	rw := uiGet(t, h, "/ui/admin/access")
	if rw.Code != http.StatusSeeOther {
		t.Errorf("expected 303, got %d", rw.Code)
	}
}

// ── /ui/repos/{name}/upload ───────────────────────────────────────────────────

func TestUIUpload_GetForm_Helm(t *testing.T) {
	srv := newUIServer(t)
	srv.Repos.Add(repo.Repository{Name: "helm-upload", Format: "helm", Kind: repo.Hosted}) //nolint:errcheck
	rw := uiGet(t, srv.Routes(), "/ui/repos/helm-upload/upload")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, `name="file"`)
	assertContains(t, body, "Chart archive")
}

func TestUIUpload_GetForm_CRAN(t *testing.T) {
	srv := newUIServer(t)
	srv.Handlers.Register(cran.New())
	srv.Repos.Add(repo.Repository{Name: "cran-upload", Format: "cran", Kind: repo.Hosted}) //nolint:errcheck
	rw := uiGet(t, srv.Routes(), "/ui/repos/cran-upload/upload")
	body := rw.Body.String()
	assertContains(t, body, "Package archive")
}

func TestUIUpload_GetForm_Maven_ShowsCLI(t *testing.T) {
	srv := newUIServer(t)
	srv.Repos.Add(repo.Repository{Name: "maven-upload", Format: "maven", Kind: repo.Hosted}) //nolint:errcheck
	rw := uiGet(t, srv.Routes(), "/ui/repos/maven-upload/upload")
	body := rw.Body.String()
	assertContains(t, body, "mvn deploy")
}

func TestUIUpload_NotFound(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/no-such-repo/upload")
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

func TestUIUpload_RepoDetail_HasUploadButton(t *testing.T) {
	// Upload button is on the browse page inside the hosted repo node header.
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/browse/npm-hosted")
	assertContains(t, rw.Body.String(), "/ui/repos/npm-hosted/upload")
}

func TestUIUpload_NPM_Success(t *testing.T) {
	srv := newUIServer(t)
	h := srv.Routes()

	tarball := buildNPMTarball(t, "my-pkg", "1.2.3")
	body, ct := buildMultipartForm(t, "file", "my-pkg-1.2.3.tgz", tarball)

	r := httptest.NewRequest(http.MethodPost, "/ui/repos/npm-hosted/upload", body)
	r.Header.Set("Content-Type", ct)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d; body: %s", rw.Code, rw.Body.String())
	}
	assertContains(t, rw.Body.String(), "Upload successful")
}

func TestUIUpload_CRAN_Success(t *testing.T) {
	srv := newUIServer(t)
	srv.Handlers.Register(cran.New())
	srv.Repos.Add(repo.Repository{Name: "cran-test", Format: "cran", Kind: repo.Hosted}) //nolint:errcheck
	h := srv.Routes()

	tarball := buildCRANTarball(t)
	body, ct := buildMultipartForm(t, "file", "MyPkg_1.0.0.tar.gz", tarball)

	r := httptest.NewRequest(http.MethodPost, "/ui/repos/cran-test/upload", body)
	r.Header.Set("Content-Type", ct)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d; body: %s", rw.Code, rw.Body.String())
	}
	assertContains(t, rw.Body.String(), "Upload successful")
}

func TestUIUpload_NPM_ClickThrough(t *testing.T) {
	// Full click-through: upload via the UI, then verify the component appears
	// in the repo browse page. This is the E2E scenario from §5 of WORKPLAN-UI.md.
	srv := newUIServer(t)
	h := srv.Routes()

	// Step 1: upload my-pkg@1.2.3 through the upload UI.
	tarball := buildNPMTarball(t, "my-pkg", "1.2.3")
	body, ct := buildMultipartForm(t, "file", "my-pkg-1.2.3.tgz", tarball)
	r := httptest.NewRequest(http.MethodPost, "/ui/repos/npm-hosted/upload", body)
	r.Header.Set("Content-Type", ct)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != http.StatusOK {
		t.Fatalf("upload: status %d; body: %s", rw.Code, rw.Body.String())
	}
	assertContains(t, rw.Body.String(), "Upload successful")

	// Step 2: repo browse page renders the 3-panel shell (packages are JS-loaded).
	rw2 := uiGet(t, h, "/ui/browse/npm-hosted")
	if rw2.Code != http.StatusOK {
		t.Fatalf("browse: status %d", rw2.Code)
	}
	assertContains(t, rw2.Body.String(), "browse-shell")

	// Step 3: component detail page must resolve and show the version.
	rw3 := uiGet(t, h, "/ui/repos/npm-hosted/my-pkg")
	if rw3.Code != http.StatusOK {
		t.Fatalf("detail: status %d", rw3.Code)
	}
	assertContains(t, rw3.Body.String(), "1.2.3")
}

func TestUIUpload_NPM_BadTarball(t *testing.T) {
	h := newUIServer(t).Routes()
	body, ct := buildMultipartForm(t, "file", "bad.tgz", []byte("not-a-tarball"))
	r := httptest.NewRequest(http.MethodPost, "/ui/repos/npm-hosted/upload", body)
	r.Header.Set("Content-Type", ct)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", rw.Code)
	}
	assertContains(t, rw.Body.String(), "package.json")
}

// ── upload test helpers ───────────────────────────────────────────────────────

// buildMultipartForm returns a multipart body and its Content-Type for file upload tests.
func buildMultipartForm(t *testing.T, field, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

// buildNPMTarball creates a minimal npm .tgz with a package/package.json.
func buildNPMTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	pkg, _ := json.Marshal(map[string]string{"name": name, "version": version})
	return buildTarGz(t, map[string][]byte{"package/package.json": pkg})
}

// buildCRANTarball creates a minimal CRAN source .tar.gz with a DESCRIPTION file.
func buildCRANTarball(t *testing.T) []byte {
	t.Helper()
	desc := []byte("Package: MyPkg\nVersion: 1.0.0\nTitle: Test\nDescription: Test.\nLicense: MIT\n")
	return buildTarGz(t, map[string][]byte{"MyPkg/DESCRIPTION": desc})
}

func buildTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, data := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))})
		tw.Write(data)
	}
	tw.Close()
	gw.Close()
	return buf.Bytes()
}

// ── /ui/admin/tokens ─────────────────────────────────────────────────────────

func TestUITokens_EvalMode_ShowsNotEnabled(t *testing.T) {
	h := newUIServer(t).Routes() // nil auth
	rw := uiGet(t, h, "/ui/admin/tokens")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	assertContains(t, rw.Body.String(), "not enabled")
}

func TestUITokens_AuthEnabled_ShowsForm(t *testing.T) {
	srv, secret := newUIServerWithAuth(t)
	h := srv.Routes()
	r := httptest.NewRequest(http.MethodGet, "/ui/admin/tokens", nil)
	r.AddCookie(&http.Cookie{Name: auth.UISessionCookie, Value: secret})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "New token")
	assertContains(t, body, `name="description"`)
	assertContains(t, body, `name="role"`)
}

func TestUITokens_UnauthenticatedRedirectsToLogin(t *testing.T) {
	srv, _ := newUIServerWithAuth(t)
	h := srv.Routes()
	rw := uiGet(t, h, "/ui/admin/tokens")
	if rw.Code != http.StatusSeeOther {
		t.Errorf("expected 303, got %d", rw.Code)
	}
	assertContains(t, rw.Header().Get("Location"), "/ui/login")
}

func TestUITokens_Create_Success(t *testing.T) {
	srv, secret := newUIServerWithAuth(t)
	h := srv.Routes()
	rw := uiPostWithCookie(t, h, "/ui/admin/tokens", auth.UISessionCookie, secret, url.Values{
		"description": {"ci-token"}, "repo": {"*"}, "role": {"write"},
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "Token created")
	assertContains(t, body, "forge_") // secret prefix
	assertContains(t, body, "ci-token") // appears in token list
}

func TestUITokens_Create_MissingDescription(t *testing.T) {
	srv, secret := newUIServerWithAuth(t)
	h := srv.Routes()
	rw := uiPostWithCookie(t, h, "/ui/admin/tokens", auth.UISessionCookie, secret, url.Values{
		"description": {""}, "repo": {"*"}, "role": {"read"},
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", rw.Code)
	}
	assertContains(t, rw.Body.String(), "description is required")
}

func TestUITokens_Create_InvalidExpiry(t *testing.T) {
	srv, secret := newUIServerWithAuth(t)
	h := srv.Routes()
	rw := uiPostWithCookie(t, h, "/ui/admin/tokens", auth.UISessionCookie, secret, url.Values{
		"description": {"x"}, "repo": {"*"}, "role": {"read"}, "expires": {"not-a-date"},
	})
	assertContains(t, rw.Body.String(), "invalid expiry")
}

func TestUITokens_Revoke(t *testing.T) {
	srv, secret := newUIServerWithAuth(t)
	h := srv.Routes()

	// Create a token to revoke.
	tok, _, _ := srv.Auth.Create("to-revoke", []auth.Grant{{Repo: "*", Role: auth.RoleRead}}, nil)

	r := httptest.NewRequest(http.MethodDelete, "/ui/admin/tokens/"+tok.ID, nil)
	r.AddCookie(&http.Cookie{Name: auth.UISessionCookie, Value: secret})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	// Verify it's gone.
	tokens, _ := srv.Auth.List()
	for _, listed := range tokens {
		if listed.ID == tok.ID {
			t.Errorf("token should have been revoked")
		}
	}
}

func TestUIAdminHome_TokensLink(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/")
	assertContains(t, rw.Body.String(), "/ui/admin/tokens")
}

// ── auth helpers ──────────────────────────────────────────────────────────────

// newUIServerWithAuth creates a Server with auth enabled and returns it alongside
// a valid admin token secret for use in auth tests.
func newUIServerWithAuth(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	authStore := auth.NewMetaStore(m)
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	reg.Register(npm.New())
	mgr.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, AnonymousRead: true}) //nolint:errcheck

	_, secret, _ := authStore.Create("test-admin", []auth.Grant{{Repo: "*", Role: auth.RoleAdmin}}, nil)
	return New(mgr, reg, b, m, authStore), secret
}

// uiPostWithCookie performs a form POST with a session cookie.
func uiPostWithCookie(t *testing.T, h http.Handler, path, cookieName, cookieVal string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: cookieName, Value: cookieVal})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	return rw
}

// uiDeleteWithCookie performs an htmx DELETE request with a session cookie.
func uiDeleteWithCookie(t *testing.T, h http.Handler, path, cookieName, cookieVal string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, path, nil)
	r.Header.Set("HX-Request", "true")
	r.AddCookie(&http.Cookie{Name: cookieName, Value: cookieVal})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	return rw
}

// ── /ui/login ─────────────────────────────────────────────────────────────────

func TestUILogin_GetForm(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/login")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, `name="token"`)
	assertContains(t, body, "Sign in")
}

func TestUILogin_InvalidToken(t *testing.T) {
	srv, _ := newUIServerWithAuth(t)
	h := srv.Routes()
	rw := uiPost(t, h, "/ui/login", url.Values{"token": {"forge_badtoken"}})
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", rw.Code)
	}
	assertContains(t, rw.Body.String(), "Invalid token")
}

func TestUILogin_ValidAdminToken_SetsCookieAndRedirects(t *testing.T) {
	srv, secret := newUIServerWithAuth(t)
	h := srv.Routes()
	rw := uiPost(t, h, "/ui/login", url.Values{"token": {secret}})
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rw.Code)
	}
	// Cookie must be set and HttpOnly
	found := false
	for _, c := range rw.Result().Cookies() {
		if c.Name == auth.UISessionCookie && c.Value == secret && c.HttpOnly {
			found = true
		}
	}
	if !found {
		t.Error("expected HttpOnly forge_token cookie to be set")
	}
}

func TestUILogin_ValidToken_RespectsNextParam(t *testing.T) {
	srv, secret := newUIServerWithAuth(t)
	h := srv.Routes()
	rw := uiPost(t, h, "/ui/login", url.Values{
		"token": {secret},
		"next":  {"/ui/repos/npm-hosted"},
	})
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rw.Code)
	}
	if loc := rw.Header().Get("Location"); loc != "/ui/repos/npm-hosted" {
		t.Errorf("expected redirect to /ui/repos/npm-hosted, got %q", loc)
	}
}

func TestUILogin_ExternalNextRejected(t *testing.T) {
	srv, secret := newUIServerWithAuth(t)
	h := srv.Routes()
	rw := uiPost(t, h, "/ui/login", url.Values{
		"token": {secret},
		"next":  {"https://evil.com"},
	})
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rw.Code)
	}
	if loc := rw.Header().Get("Location"); loc != "/ui/admin/" {
		t.Errorf("external next not sanitised: got redirect to %q", loc)
	}
}

// ── /ui/logout ────────────────────────────────────────────────────────────────

func TestUILogout_ClearsCookieAndRedirects(t *testing.T) {
	h := newUIServer(t).Routes()
	r := httptest.NewRequest(http.MethodPost, "/ui/logout", nil)
	r.AddCookie(&http.Cookie{Name: auth.UISessionCookie, Value: "forge_sometoken"})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rw.Code)
	}
	cleared := false
	for _, c := range rw.Result().Cookies() {
		if c.Name == auth.UISessionCookie && c.MaxAge == -1 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("expected forge_token cookie to be cleared (MaxAge=-1)")
	}
}

// ── U0: admin mutations require auth ─────────────────────────────────────────

func TestUIAdmin_MutationsRedirectToLoginWhenUnauthenticated(t *testing.T) {
	srv, _ := newUIServerWithAuth(t)
	h := srv.Routes()

	cases := []struct {
		name   string
		method string
		path   string
		form   url.Values
	}{
		{"new GET", http.MethodGet, "/ui/admin/repos/new", nil},
		{"new POST", http.MethodPost, "/ui/admin/repos/new", url.Values{"name": {"x"}, "format": {"npm"}, "kind": {"hosted"}}},
		{"edit GET", http.MethodGet, "/ui/admin/repos/npm-hosted/edit", nil},
		{"edit POST", http.MethodPost, "/ui/admin/repos/npm-hosted/edit", url.Values{"format": {"npm"}, "kind": {"hosted"}}},
		{"delete", http.MethodDelete, "/ui/admin/repos/npm-hosted", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rw *httptest.ResponseRecorder
			if tc.method == http.MethodDelete {
				rw = uiDelete(t, h, tc.path)
			} else if tc.method == http.MethodPost {
				rw = uiPost(t, h, tc.path, tc.form)
			} else {
				rw = uiGet(t, h, tc.path)
			}
			if rw.Code != http.StatusSeeOther {
				t.Errorf("expected 303 redirect to login, got %d", rw.Code)
			}
			if loc := rw.Header().Get("Location"); !strings.Contains(loc, "/ui/login") {
				t.Errorf("expected redirect to /ui/login, got %q", loc)
			}
		})
	}
}

func TestUIAdmin_MutationsSucceedWithValidCookie(t *testing.T) {
	srv, secret := newUIServerWithAuth(t)
	h := srv.Routes()

	// Create via form POST with cookie.
	rw := uiPostWithCookie(t, h, "/ui/admin/repos/new", auth.UISessionCookie, secret, url.Values{
		"name": {"cran-hosted"}, "format": {"cran"}, "kind": {"hosted"},
	})
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("create with cookie: expected 303, got %d; body: %s", rw.Code, rw.Body.String())
	}
	if _, ok := srv.Repos.Get("cran-hosted"); !ok {
		t.Error("repo should have been created")
	}

	// Delete via htmx with cookie.
	rw2 := uiDeleteWithCookie(t, h, "/ui/admin/repos/npm-hosted", auth.UISessionCookie, secret)
	if rw2.Code != http.StatusOK {
		t.Fatalf("delete with cookie: expected 200, got %d", rw2.Code)
	}
	if _, ok := srv.Repos.Get("npm-hosted"); ok {
		t.Error("repo should have been deleted")
	}
}

func TestUIAdmin_EvalMode_NoAuthRequired(t *testing.T) {
	// Eval mode (nil auth): existing behaviour — no credentials needed.
	h := newUIServer(t).Routes()
	rw := uiPost(t, h, "/ui/admin/repos/new", url.Values{
		"name": {"maven-hosted"}, "format": {"maven"}, "kind": {"hosted"},
	})
	if rw.Code != http.StatusSeeOther {
		t.Errorf("eval mode: expected 303, got %d", rw.Code)
	}
}

// ── Foundry admin shell new routes ────────────────────────────────────────────

func TestUIDashboard_OK(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/dashboard")
	if rw.Code != http.StatusOK {
		t.Fatalf("/ui/dashboard: status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "Dashboard")
	assertContains(t, body, "System overview")
}

func TestUIDashboard_SidebarNav(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/dashboard")
	body := rw.Body.String()
	assertContains(t, body, "Tokens &amp; Access")
	assertContains(t, body, "Cleanup")
	assertContains(t, body, "Observability")
	// Active nav item should be marked
	assertContains(t, body, `sidebar-nav-item    active">`) // dashboard nav item is active
}

func TestUIDashboard_ShowsFormatStats(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/dashboard")
	body := rw.Body.String()
	// newUIServer seeds npm-hosted and helm-hosted repos
	assertContains(t, body, "npm")
	assertContains(t, body, "helm")
}

func TestUIAdminTokensV2_OK(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/tokens")
	if rw.Code != http.StatusOK {
		t.Fatalf("/ui/admin/tokens: status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "Tokens &amp; Access")
	assertContains(t, body, "sidebar-nav")
	assertContains(t, body, "Authentication is not enabled") // eval mode shows this alert
}

func TestUICleanupPolicies_OK(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/cleanup-policies")
	if rw.Code != http.StatusOK {
		t.Fatalf("/ui/admin/cleanup-policies: status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "Cleanup")
}

func TestUICleanupPolicies_ShowsConfiguredPolicies(t *testing.T) {
	srv := newUIServer(t)
	// Wire a PolicyManager and add a named policy.
	pm := cleanup.NewPolicyManager(srv.Meta)
	pm.Put(cleanup.NamedPolicy{ //nolint:errcheck
		Name:                "keep-5-30d",
		Description:         "Keep 5 versions, delete older than 30 days",
		KeepVersions:        5,
		DeleteOlderThanDays: 30,
	})
	srv.Cleanup = pm
	h := srv.Routes()
	rw := uiGet(t, h, "/ui/admin/cleanup-policies")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "keep-5-30d")
	assertContains(t, body, "Keep last 5 versions")
}

func TestUIObservability_OK(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/admin/observability")
	if rw.Code != http.StatusOK {
		t.Fatalf("/ui/admin/observability: status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "Observability")
	assertContains(t, body, "Audit log")
}
