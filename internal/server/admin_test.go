package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

// newAdminServer builds a Server wired for admin API tests.
// Auth is disabled (eval mode) so RequireAdmin always passes.
func newAdminServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	return New(mgr, reg, b, m, nil) // nil auth = eval mode
}

func adminReq(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestAdminRepos_ListEmpty(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/repos", nil))

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	var repos []repo.Repository
	json.NewDecoder(rw.Body).Decode(&repos)
	if len(repos) != 0 {
		t.Errorf("expected empty list, got %d repos", len(repos))
	}
}

func TestAdminRepos_CreateAndGet(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	// Create a hosted repo.
	body := map[string]any{
		"name": "npm-hosted", "format": "npm", "kind": "hosted", "anonymousRead": true,
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos", body))
	if rw.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", rw.Code, rw.Body.String())
	}

	// Get it back.
	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, adminReq(t, http.MethodGet, "/api/v1/repos/npm-hosted", nil))
	if rw2.Code != http.StatusOK {
		t.Fatalf("get status %d", rw2.Code)
	}
	var r repo.Repository
	json.NewDecoder(rw2.Body).Decode(&r)
	if r.Name != "npm-hosted" || r.Format != "npm" || r.Kind != repo.Hosted {
		t.Errorf("unexpected repo: %+v", r)
	}
}

func TestAdminRepos_CreateProxy(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	body := map[string]any{
		"name": "npm-proxy", "format": "npm", "kind": "proxy",
		"upstream": "https://registry.npmjs.org", "anonymousRead": true,
		"proxyTTL": "12h",
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos", body))
	if rw.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
	}
	var r repo.Repository
	json.NewDecoder(rw.Body).Decode(&r)
	if r.Upstream != "https://registry.npmjs.org" {
		t.Errorf("upstream not set: %q", r.Upstream)
	}
}

func TestAdminRepos_Update(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	// Create first.
	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPost, "/api/v1/repos",
		map[string]any{"name": "helm-hosted", "format": "helm", "kind": "hosted"}))

	// Update anonymousRead.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPut, "/api/v1/repos/helm-hosted",
		map[string]any{"format": "helm", "kind": "hosted", "anonymousRead": true}))
	if rw.Code != http.StatusOK {
		t.Fatalf("update status %d: %s", rw.Code, rw.Body.String())
	}

	r, _ := srv.Repos.Get("helm-hosted")
	if !r.AnonymousRead {
		t.Error("expected anonymousRead=true after update")
	}
}

func TestAdminRepos_Delete(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPost, "/api/v1/repos",
		map[string]any{"name": "tmp-repo", "format": "maven", "kind": "hosted"}))

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodDelete, "/api/v1/repos/tmp-repo", nil))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("delete status %d", rw.Code)
	}

	if _, ok := srv.Repos.Get("tmp-repo"); ok {
		t.Error("repo should be deleted")
	}
}

func TestAdminRepos_Validation(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	cases := []struct {
		desc   string
		body   map[string]any
		status int
	}{
		{"missing name", map[string]any{"format": "npm", "kind": "hosted"}, http.StatusBadRequest},
		{"missing format", map[string]any{"name": "x", "kind": "hosted"}, http.StatusBadRequest},
		{"invalid kind", map[string]any{"name": "x", "format": "npm", "kind": "bogus"}, http.StatusBadRequest},
		{"proxy without upstream", map[string]any{"name": "x", "format": "npm", "kind": "proxy"}, http.StatusBadRequest},
		{"group without members", map[string]any{"name": "x", "format": "npm", "kind": "group"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos", tc.body))
		if rw.Code != tc.status {
			t.Errorf("%s: got %d, want %d", tc.desc, rw.Code, tc.status)
		}
	}
}

func TestAdminRepos_DuplicateCreate(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	body := map[string]any{"name": "dup", "format": "npm", "kind": "hosted"}
	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPost, "/api/v1/repos", body))

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos", body))
	if rw.Code != http.StatusConflict {
		t.Errorf("expected 409 on duplicate, got %d", rw.Code)
	}
}

func TestAdminRepos_AuthRequired(t *testing.T) {
	// With auth enabled, requests without a token must get 401.
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	authStore := auth.NewMetaStore(m)
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	srv := New(mgr, reg, b, m, authStore) // auth ENABLED

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil))
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", rw.Code)
	}
}

// ── SEC-002: group anonymousRead policy ───────────────────────────────────────

func TestGroupPolicy_PublicGroupPrivateMember_Rejected(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	// Create a private member repo.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "npm-private", "format": "npm", "kind": "hosted",
		"anonymousRead": false,
	}))
	if rw.Code != http.StatusCreated {
		t.Fatalf("create private repo: %d %s", rw.Code, rw.Body)
	}

	// Creating a public group that includes the private member must be rejected.
	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "npm-public-group", "format": "npm", "kind": "group",
		"members": []string{"npm-private"}, "anonymousRead": true,
	}))
	if rw2.Code != http.StatusBadRequest {
		t.Errorf("public group over private member: got %d want 400", rw2.Code)
	}
}

func TestGroupPolicy_PublicGroupPublicMember_Allowed(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "npm-pub", "format": "npm", "kind": "hosted", "anonymousRead": true,
	}))

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "npm-pub-group", "format": "npm", "kind": "group",
		"members": []string{"npm-pub"}, "anonymousRead": true,
	}))
	if rw.Code != http.StatusCreated {
		t.Errorf("public group + public member: got %d want 201", rw.Code)
	}
}

func TestGroupPolicy_PrivateGroupPrivateMember_Allowed(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "npm-priv2", "format": "npm", "kind": "hosted", "anonymousRead": false,
	}))

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "npm-priv-group", "format": "npm", "kind": "group",
		"members": []string{"npm-priv2"}, "anonymousRead": false,
	}))
	if rw.Code != http.StatusCreated {
		t.Errorf("private group + private member: got %d want 201", rw.Code)
	}
}

func TestMemberPolicy_MakePrivateBlockedByPublicGroup(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	// Set up: public member inside a public group.
	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "npm-member", "format": "npm", "kind": "hosted", "anonymousRead": true,
	}))
	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "npm-group", "format": "npm", "kind": "group",
		"members": []string{"npm-member"}, "anonymousRead": true,
	}))

	// Attempt to make the member private while the group is still public.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPut, "/api/v1/repos/npm-member", map[string]any{
		"name": "npm-member", "format": "npm", "kind": "hosted", "anonymousRead": false,
	}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("making member private while in public group: got %d want 400", rw.Code)
	}
}

func TestAdminRepos_DeleteNotFound(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, adminReq(t, http.MethodDelete, "/api/v1/repos/ghost", nil))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("delete nonexistent: expected 404, got %d", rw.Code)
	}
}

func TestAdminRepos_UpdateNotFound(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, adminReq(t, http.MethodPut, "/api/v1/repos/ghost", map[string]any{
		"name": "ghost", "format": "npm", "kind": "hosted",
	}))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("update nonexistent: expected 404, got %d", rw.Code)
	}
}

func TestMemberPolicy_MakePrivateAfterGroupFixed_Allowed(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	// Set up: public member inside a public group.
	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "npm-mem2", "format": "npm", "kind": "hosted", "anonymousRead": true,
	}))
	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "npm-grp2", "format": "npm", "kind": "group",
		"members": []string{"npm-mem2"}, "anonymousRead": true,
	}))

	// First fix the group (make it private).
	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPut, "/api/v1/repos/npm-grp2", map[string]any{
		"name": "npm-grp2", "format": "npm", "kind": "group",
		"members": []string{"npm-mem2"}, "anonymousRead": false,
	}))

	// Now making the member private must be allowed.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPut, "/api/v1/repos/npm-mem2", map[string]any{
		"name": "npm-mem2", "format": "npm", "kind": "hosted", "anonymousRead": false,
	}))
	if rw.Code != http.StatusOK {
		t.Errorf("making member private after group is fixed: got %d want 200", rw.Code)
	}
}
