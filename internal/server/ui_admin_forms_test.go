package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestUITokensTab_UsersAndRoles drives the Access page's user/role tabs and
// their form processors end to end on an eval-mode server with the user + role
// stores wired.
func TestUITokensTab_UsersAndRoles(t *testing.T) {
	srv := newRichUIServer(t)
	h := srv.Routes()

	// GET the users tab — renders buildUsersTabData / buildRolesTabData / roleClass.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/ui/admin/tokens?tab=users", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET tokens?tab=users: status %d (%.200s)", rw.Code, rw.Body.String())
	}

	// POST invite a user.
	rw = uiPost(t, h, "/ui/admin/tokens?tab=users", url.Values{
		"username": {"dave"}, "password": {"dave-pass-1234"}, "role": {"Publisher"},
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("invite user: status %d (%.200s)", rw.Code, rw.Body.String())
	}
	if _, ok, _ := srv.Users.Get("dave"); !ok {
		t.Error("invite form should have created user dave")
	}

	// Invite with missing fields → error message rendered (still 200).
	rw = uiPost(t, h, "/ui/admin/tokens?tab=users", url.Values{"username": {"x"}})
	if rw.Code != http.StatusOK {
		t.Errorf("invalid invite: status %d, want 200 with error", rw.Code)
	}

	// POST create a role.
	rw = uiPost(t, h, "/ui/admin/tokens?tab=roles", url.Values{
		"name": {"Curator"}, "description": {"curates"}, "baseRole": {"write"},
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("create role: status %d (%.200s)", rw.Code, rw.Body.String())
	}

	// Role with no name → error rendered.
	rw = uiPost(t, h, "/ui/admin/tokens?tab=roles", url.Values{"description": {"nameless"}})
	if rw.Code != http.StatusOK {
		t.Errorf("nameless role: status %d, want 200 with error", rw.Code)
	}
}

// TestUIToggleAndDeleteUser drives the user disable-toggle and delete htmx
// actions.
func TestUIToggleAndDeleteUser(t *testing.T) {
	srv := newRichUIServer(t)
	h := srv.Routes()
	srv.Users.Create("erin", "erin-pass-1234", "Reader") //nolint:errcheck

	// Toggle disable → redirect back to the users tab, user now disabled.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/ui/admin/tokens/users/erin/disable", nil))
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("toggle: status %d, want 303", rw.Code)
	}
	if u, _, _ := srv.Users.Get("erin"); !u.Disabled {
		t.Error("erin should be disabled after toggle")
	}

	// Delete erin via htmx DELETE.
	rw = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/ui/admin/tokens/users/erin", nil)
	req.Header.Set("HX-Request", "true")
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("delete user: status %d", rw.Code)
	}
	if _, ok, _ := srv.Users.Get("erin"); ok {
		t.Error("erin should be gone after delete")
	}

	// Toggling a missing user → 404.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/ui/admin/tokens/users/ghost/disable", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("toggle missing user: status %d, want 404", rw.Code)
	}
}
