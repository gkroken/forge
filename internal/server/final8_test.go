package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"forge/internal/obs"
)

// TestAuditAPI_DurableCursor drives GET /api/v1/audit against a durable querier
// so the keyset-cursor branch (X-Next-Cursor headers) is covered.
func TestAuditAPI_DurableCursor(t *testing.T) {
	srv := newAdminServer(t)
	q := &fakeQuerier{}
	now := time.Now().UTC()
	// Exactly `limit` rows → the handler emits the next-page cursor headers.
	for i := 0; i < 50; i++ {
		q.rows = append(q.rows, obs.AuditRecord{
			AuditEntry: obs.AuditEntry{Timestamp: now, Actor: "alice", Method: "PUT", Path: "/repository/npm-hosted/p", Status: 201},
			ID:         int64(50 - i),
		})
	}
	srv.WithAuditLog(q)

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/audit?limit=50", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("audit api durable: status %d", rw.Code)
	}
	if rw.Header().Get("X-Next-Cursor-Id") == "" {
		t.Error("expected X-Next-Cursor-Id header when a full page is returned")
	}
}

// TestAdminSecurity_MethodBranches covers the security-policy dispatchers'
// list/default method-not-allowed branches.
func TestAdminSecurity_MethodBranches(t *testing.T) {
	srv := newRichUIServer(t)
	h := srv.Routes()

	// Collection: unsupported method → 405.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodDelete, "/api/v1/security-policies", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE security-policies collection: %d, want 405", rw.Code)
	}

	// Named policy: unsupported method → 405.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPatch, "/api/v1/security-policies/x", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH named policy: %d, want 405", rw.Code)
	}

	// Not configured → 503.
	bare := newAdminServer(t)
	rw = httptest.NewRecorder()
	bare.Routes().ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/security-policies", nil))
	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("security-policies no manager: %d, want 503", rw.Code)
	}
}
