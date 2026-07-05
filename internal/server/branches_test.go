package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/cleanup"
	"forge/internal/repo"
)

func TestClaimedName(t *testing.T) {
	s := newMigrationServer(t) // registers npm
	if err := s.Repos.Add(repo.Repository{
		Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, Claims: []string{"@acme/*"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Repos.Add(repo.Repository{
		Name: "npm-proxy", Format: "npm", Kind: repo.Proxy, Upstream: "https://registry.npmjs.org",
	}); err != nil {
		t.Fatal(err)
	}
	proxy, _ := s.Repos.Get("npm-proxy")
	h, _ := s.Handlers.For("npm")
	g := s.newDepGuard(proxy, h, "@acme/widget")
	if g == nil {
		t.Fatal("expected a dep guard for a proxy with a claiming hosted member")
	}
	if !g.claimedName("@acme/widget") {
		t.Error("@acme/widget should be claimed")
	}
	if g.claimedName("lodash") {
		t.Error("lodash should not be claimed")
	}
}

// TestMethodAndErrorBranches covers assorted method-not-allowed / not-enabled /
// error branches that the happy-path tests skip.
func TestMethodAndErrorBranches(t *testing.T) {
	// handleBlobStores: non-GET → 405.
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/blob-stores", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST blob-stores: %d, want 405", rw.Code)
	}

	// handleSecurityDefault: unsupported method → 405.
	rich := newRichUIServer(t)
	rw = httptest.NewRecorder()
	rich.Routes().ServeHTTP(rw, adminReq(t, http.MethodDelete, "/api/v1/security-policies/_default", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE _default: %d, want 405", rw.Code)
	}

	// handleWebhooks collection: unsupported method → 405.
	whsrv, _, _ := newLocalWebhookServer(t)
	rw = httptest.NewRecorder()
	whsrv.Routes().ServeHTTP(rw, adminReq(t, http.MethodPut, "/api/v1/webhooks", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT webhooks collection: %d, want 405", rw.Code)
	}

	// uiAdminDeleteUser with user mgmt disabled → 501.
	rw = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/ui/admin/tokens/users/nobody", nil)
	req.Header.Set("HX-Request", "true")
	srv.Routes().ServeHTTP(rw, req) // srv has no Users store
	if rw.Code != http.StatusNotImplemented {
		t.Errorf("delete user (no store): %d, want 501", rw.Code)
	}

	// uiAdminRevokeToken with auth disabled → 501.
	rw = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/ui/admin/tokens/sometoken", nil)
	req.Header.Set("HX-Request", "true")
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusNotImplemented {
		t.Errorf("revoke token (no auth): %d, want 501", rw.Code)
	}
}

// TestRoleAPI_ErrorBranches covers apiCreateRole duplicate and apiDeleteRole
// missing.
func TestRoleAPI_ErrorBranches(t *testing.T) {
	srv := newUserMgmtServer(t)
	h := srv.Routes()

	// Create then duplicate → 409.
	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPost, "/api/v1/roles",
		map[string]any{"name": "Dup"}))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/roles", map[string]any{"name": "Dup"}))
	if rw.Code != http.StatusConflict {
		t.Errorf("duplicate role: %d, want 409", rw.Code)
	}

	// Delete the created role → 204.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodDelete, "/api/v1/roles/Dup", nil))
	if rw.Code != http.StatusNoContent {
		t.Errorf("delete role: %d, want 204", rw.Code)
	}
}

// TestDeleteCleanupPolicy_NonHTMX covers the plain-redirect branch.
func TestDeleteCleanupPolicy_NonHTMX(t *testing.T) {
	srv := newRichUIServer(t)
	srv.Cleanup.Put(cleanup.NamedPolicy{Name: "temp"}) //nolint:errcheck
	rw := httptest.NewRecorder()
	// No HX-Request header → 303 redirect.
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodDelete, "/ui/admin/cleanup-policies/temp", nil))
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("non-htmx delete: %d, want 303", rw.Code)
	}
}
