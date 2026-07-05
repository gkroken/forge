package server

import (
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

// newUserMgmtServer builds an eval-mode server (auth off → RequireAdmin passes)
// that nonetheless has the user + role stores wired, so the /api/v1/users and
// /api/v1/roles handlers run their real logic instead of the 501 short-circuit.
func newUserMgmtServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	srv := New(repo.NewManager(), format.NewRegistry(), b, m, nil)
	return srv.WithUsers(auth.NewUserStore(m)).WithRoles(auth.NewRoleStore(m))
}

func TestUsersAPI_NotEnabled_501(t *testing.T) {
	srv := newAdminServer(t) // no Users store
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/users", nil))
	if rw.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501 when user mgmt disabled", rw.Code)
	}
}

func TestUsersAPI_CRUDLifecycle(t *testing.T) {
	srv := newUserMgmtServer(t)
	h := srv.Routes()

	// Create.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/users", map[string]any{
		"username": "alice", "password": "s3cret-pass", "role": "Publisher",
	}))
	if rw.Code != http.StatusCreated {
		t.Fatalf("create: status %d (%s)", rw.Code, rw.Body.String())
	}

	// List — alice present.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/users", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("list: status %d", rw.Code)
	}
	var users []auth.User
	json.NewDecoder(rw.Body).Decode(&users)
	if len(users) != 1 || users[0].Username != "alice" {
		t.Fatalf("list: got %+v, want [alice]", users)
	}

	// Update role + disable + password.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPut, "/api/v1/users/alice", map[string]any{
		"role": "Reader", "disabled": true, "password": "new-pass-1234",
	}))
	if rw.Code != http.StatusOK {
		t.Fatalf("update: status %d (%s)", rw.Code, rw.Body.String())
	}
	var updated auth.User
	json.NewDecoder(rw.Body).Decode(&updated)
	if updated.Role != "Reader" || !updated.Disabled {
		t.Errorf("update: got role=%q disabled=%v, want Reader/true", updated.Role, updated.Disabled)
	}

	// Delete.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodDelete, "/api/v1/users/alice", nil))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d", rw.Code)
	}

	// List is now empty — the user is gone.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/users", nil))
	json.NewDecoder(rw.Body).Decode(&users)
	if len(users) != 0 {
		t.Errorf("after delete: %d users remain, want 0", len(users))
	}

	// PUT to a missing user → 404.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPut, "/api/v1/users/ghost", map[string]any{"role": "Reader"}))
	if rw.Code != http.StatusNotFound {
		t.Errorf("update missing user: status %d, want 404", rw.Code)
	}
}

func TestUsersAPI_CreateValidation(t *testing.T) {
	srv := newUserMgmtServer(t)
	h := srv.Routes()

	// Missing password.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/users", map[string]any{"username": "bob"}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("missing password: status %d, want 400", rw.Code)
	}

	// Duplicate username → 409.
	h.ServeHTTP(httptest.NewRecorder(), adminReq(t, http.MethodPost, "/api/v1/users",
		map[string]any{"username": "carol", "password": "pw-carol-123"}))
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/users",
		map[string]any{"username": "carol", "password": "pw-carol-456"}))
	if rw.Code != http.StatusConflict {
		t.Errorf("duplicate user: status %d, want 409", rw.Code)
	}
}

func TestRolesAPI_NotEnabled_501(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/roles", nil))
	if rw.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501 when role mgmt disabled", rw.Code)
	}
}

func TestRolesAPI_CRUDLifecycle(t *testing.T) {
	srv := newUserMgmtServer(t)
	h := srv.Routes()

	// List — predefined roles always present, custom empty.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/roles", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("list: status %d", rw.Code)
	}
	var resp rolesResponse
	json.NewDecoder(rw.Body).Decode(&resp)
	if len(resp.Predefined) == 0 {
		t.Error("expected predefined roles in listing")
	}

	// Create a custom role.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/roles", map[string]any{
		"name":        "Releaser",
		"description": "can promote",
		"grants":      []map[string]any{{"repo": "*", "actions": []string{"read", "write"}}},
	}))
	if rw.Code != http.StatusCreated {
		t.Fatalf("create role: status %d (%s)", rw.Code, rw.Body.String())
	}

	// It appears in the custom list.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/roles", nil))
	json.NewDecoder(rw.Body).Decode(&resp)
	var found bool
	for _, r := range resp.Custom {
		if r.Name == "Releaser" {
			found = true
		}
	}
	if !found {
		t.Errorf("custom role Releaser not in listing: %+v", resp.Custom)
	}

	// Delete it.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodDelete, "/api/v1/roles/Releaser", nil))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("delete role: status %d", rw.Code)
	}
}
