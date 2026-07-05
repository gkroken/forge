package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/obs"
)

// TestPromoteHTTP_HappyAndErrors drives POST /api/v1/repos/{target}/promote,
// covering handlePromote, recordPromotion (audit path), and the promoteError →
// HTTP-status mapping.
func TestPromoteHTTP_HappyAndErrors(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "npm-stage", "npm")
	addHosted(t, s, "npm-prod", "npm")
	s.WithAuditLog(obs.NewAuditLog(10))
	h := s.Routes()

	tarball := []byte("promoted tarball bytes")
	seedBlobBytes(t, s, http.MethodPut, "npm-stage", "leftpad", npmPublishBody("leftpad", "1.2.3", tarball))

	// Happy path.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos/npm-prod/promote", map[string]any{
		"sourceRepo": "npm-stage", "component": "leftpad", "version": "1.2.3",
	}))
	if rw.Code != http.StatusOK {
		t.Fatalf("promote: status %d (%s)", rw.Code, rw.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rw.Body).Decode(&resp)
	if resp["promoted"] != true {
		t.Errorf("promote response = %v, want promoted:true", resp)
	}
	if _, ok, _ := s.Blob.Stat("npm-prod/leftpad/-/leftpad-1.2.3.tgz"); !ok {
		t.Error("promoted tarball missing from target")
	}

	// Missing sourceRepo → 400.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos/npm-prod/promote", map[string]any{
		"component": "leftpad", "version": "1.2.3",
	}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("missing sourceRepo: status %d, want 400", rw.Code)
	}

	// Source lacks the version → promoteError 404 mapped through.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos/npm-prod/promote", map[string]any{
		"sourceRepo": "npm-stage", "component": "leftpad", "version": "9.9.9",
	}))
	if rw.Code != http.StatusNotFound {
		t.Errorf("missing version: status %d, want 404", rw.Code)
	}

	// Unknown target → 404.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos/ghost/promote", map[string]any{
		"sourceRepo": "npm-stage", "component": "leftpad", "version": "1.2.3",
	}))
	if rw.Code != http.StatusNotFound {
		t.Errorf("unknown target: status %d, want 404", rw.Code)
	}
}
