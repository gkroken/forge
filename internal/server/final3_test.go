package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestBrowseVersions_Branches covers the guard branches of uiBrowseVersions.
func TestBrowseVersions_Branches(t *testing.T) {
	srv := newUIServer(t)
	h := srv.Routes()

	// Wrong method → 405.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/ui/browse/npm-hosted/versions?pkg=lodash", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST versions: %d, want 405", rw.Code)
	}
	// Missing repo → 404.
	rw = uiGet(t, h, "/ui/browse/ghost/versions?pkg=lodash")
	if rw.Code != http.StatusNotFound {
		t.Errorf("missing repo: %d, want 404", rw.Code)
	}
	// Missing pkg → 400.
	rw = uiGet(t, h, "/ui/browse/npm-hosted/versions")
	if rw.Code != http.StatusBadRequest {
		t.Errorf("missing pkg: %d, want 400", rw.Code)
	}
	// Unknown component → 404.
	rw = uiGet(t, h, "/ui/browse/npm-hosted/versions?pkg=does-not-exist")
	if rw.Code != http.StatusNotFound {
		t.Errorf("unknown pkg: %d, want 404", rw.Code)
	}
}

// TestRoleForm_Duplicate covers processRoleCreateForm's Create-error branch.
func TestRoleForm_Duplicate(t *testing.T) {
	srv := newRichUIServer(t)
	h := srv.Routes()
	form := url.Values{"name": {"Dupe"}, "description": {"first"}}
	if rw := uiPost(t, h, "/ui/admin/tokens?tab=roles", form); rw.Code != http.StatusOK {
		t.Fatalf("first create: %d", rw.Code)
	}
	// Second create with the same name → the error message is rendered (still 200).
	rw := uiPost(t, h, "/ui/admin/tokens?tab=roles", form)
	if rw.Code != http.StatusOK {
		t.Errorf("duplicate role form: %d, want 200 with error", rw.Code)
	}
}

// TestMigrationPlan_BadBody covers migrationPlanAPI's decode-error branch.
func TestMigrationPlan_BadBody(t *testing.T) {
	s := newMigrationServer(t)
	rw := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migration/plan", strings.NewReader("{not json"))
	s.Routes().ServeHTTP(rw, req)
	if rw.Code < 400 {
		t.Errorf("bad plan body: status %d, want a 4xx", rw.Code)
	}
}
