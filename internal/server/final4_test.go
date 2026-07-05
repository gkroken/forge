package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/queue"
	"forge/internal/vuln"
)

// TestRepoSecurityPolicy_Assign covers handleRepoSecurityPolicy (GET resolved,
// PUT assign valid + invalid) and handleRepoSecurityDryRun.
func TestRepoSecurityPolicy_Assign(t *testing.T) {
	srv := newRichUIServer(t) // has VulnPolicy + npm-hosted
	srv.VulnPolicy.Put(vuln.NamedPolicy{Name: "strict", Policy: vuln.Policy{Mode: vuln.ModeBlock}}) //nolint:errcheck
	h := srv.Routes()

	// GET the resolved policy (default).
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/repos/npm-hosted/security-policy", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("get resolved: status %d", rw.Code)
	}

	// PUT assign an unknown policy → 400.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPut, "/api/v1/repos/npm-hosted/security-policy",
		map[string]any{"policyName": "ghost"}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("assign unknown: status %d, want 400", rw.Code)
	}

	// PUT assign the real policy → 200.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPut, "/api/v1/repos/npm-hosted/security-policy",
		map[string]any{"policyName": "strict"}))
	if rw.Code != http.StatusOK {
		t.Fatalf("assign strict: status %d (%s)", rw.Code, rw.Body.String())
	}
	if rp, _ := srv.Repos.Get("npm-hosted"); rp.SecurityPolicyName != "strict" {
		t.Errorf("policy not assigned: %q", rp.SecurityPolicyName)
	}

	// Dry-run preview (200 when the findings store is wired, 503 otherwise —
	// either way the handler runs; this harness has no Vuln store).
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos/npm-hosted/security-policy/dry-run",
		map[string]any{"policyName": "strict"}))
	if rw.Code != http.StatusOK && rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("dry-run: status %d (%s)", rw.Code, rw.Body.String())
	}
}

// TestSystemTasks_WithRing covers systemTasks' non-empty branch.
func TestSystemTasks_WithRing(t *testing.T) {
	srv := newAdminServer(t)
	srv.TaskRing = queue.NewTaskRing(4)
	srv.TaskRing.Wrap(func(context.Context, queue.Job) error { return nil })(context.Background(), queue.Job{Type: "x"})

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/system/tasks", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("system tasks: status %d", rw.Code)
	}
}

// TestCreateUser_DefaultRole covers apiCreateUser's empty-role → Reader default.
func TestCreateUser_DefaultRole(t *testing.T) {
	srv := newUserMgmtServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/users",
		map[string]any{"username": "norole", "password": "pw-norole-123"}))
	if rw.Code != http.StatusCreated {
		t.Fatalf("create no-role user: status %d (%s)", rw.Code, rw.Body.String())
	}
	if u, ok, _ := srv.Users.Get("norole"); !ok || u.Role != "Reader" {
		t.Errorf("default role = %q, want Reader", u.Role)
	}
}
