package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/npm-hosted")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	assertContains(t, body, "npm-hosted")
	assertContains(t, body, "lodash")
	assertContains(t, body, "4.17.21") // latest version
}

func TestUIRepo_NotFound(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/no-such-repo")
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

func TestUIRepo_HasFilterInput(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/npm-hosted")
	body := rw.Body.String()
	// htmx filter input must be present
	assertContains(t, body, `hx-get="/ui/repos/npm-hosted"`)
	assertContains(t, body, `hx-target="#components-section"`)
}

func TestUIRepo_HtmxPartial(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/npm-hosted?q=lodash", "HX-Request", "true")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	body := rw.Body.String()
	// Partial must NOT have the base layout
	assertNotContains(t, body, "<!DOCTYPE html>")
	assertNotContains(t, body, "<nav")
	// But must contain the component
	assertContains(t, body, "lodash")
}

func TestUIRepo_HtmxFilter_NoMatch(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/npm-hosted?q=zzznomatch", "HX-Request", "true")
	body := rw.Body.String()
	assertContains(t, body, "zzznomatch") // shown in empty-state message
	assertNotContains(t, body, "lodash")
}

func TestUIRepo_EmptyRepo(t *testing.T) {
	h := newUIServer(t).Routes()
	rw := uiGet(t, h, "/ui/repos/helm-hosted") // no seeded data for helm beyond one chart
	body := rw.Body.String()
	// mychart IS seeded
	assertContains(t, body, "mychart")
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
