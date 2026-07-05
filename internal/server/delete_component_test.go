package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/cleanup"
	"forge/internal/obs"
)

// TestDeleteComponent_TrashLifecycle publishes a real npm package, soft-deletes
// it (→ trash + audit row), then lists / restores / purges the trash. This
// exercises handleDeleteComponent, recordRepoAudit, actorLabel and handleTrash
// end to end.
func TestDeleteComponent_TrashLifecycle(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "npm-hosted", "npm")
	al := obs.NewAuditLog(20)
	s.WithAuditLog(al)
	h := s.Routes()

	tarball := []byte("fake npm tarball bytes")
	seedBlobBytes(t, s, http.MethodPut, "npm-hosted", "leftpad", npmPublishBody("leftpad", "1.2.3", tarball))

	// Soft-delete the component.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodDelete,
		"/api/v1/repos/npm-hosted/component?name=leftpad&version=1.2.3", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("delete component: status %d (%s)", rw.Code, rw.Body.String())
	}
	var delResp map[string]any
	json.NewDecoder(rw.Body).Decode(&delResp)
	if delResp["deleted"] != float64(1) || delResp["restorable"] != true {
		t.Errorf("delete response = %v, want deleted:1 restorable:true", delResp)
	}
	// The tarball blob is gone from the live key space.
	if _, ok, _ := s.Blob.Stat("npm-hosted/leftpad/-/leftpad-1.2.3.tgz"); ok {
		t.Error("tarball should have moved to trash")
	}
	// An audit row was recorded.
	if len(al.Recent(5)) == 0 {
		t.Error("delete should have recorded an audit row")
	}

	// List the trash → one tombstone.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-hosted/trash", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("trash list: status %d", rw.Code)
	}
	var trashResp struct {
		Trash []cleanup.Tombstone `json:"trash"`
	}
	json.NewDecoder(rw.Body).Decode(&trashResp)
	if len(trashResp.Trash) != 1 {
		t.Fatalf("trash: got %d tombstones, want 1", len(trashResp.Trash))
	}
	trashID := trashResp.Trash[0].ID

	// Restore it → tarball returns to the live key space.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/npm-hosted/trash/restore?id="+trashID, nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("restore: status %d (%s)", rw.Code, rw.Body.String())
	}
	if _, ok, _ := s.Blob.Stat("npm-hosted/leftpad/-/leftpad-1.2.3.tgz"); !ok {
		t.Error("tarball should be restored to the live key space")
	}

	// Delete again then hard-purge all.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodDelete,
		"/api/v1/repos/npm-hosted/component?name=leftpad&version=1.2.3", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("re-delete: status %d", rw.Code)
	}
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/npm-hosted/trash/purge?id=all", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("purge all: status %d (%s)", rw.Code, rw.Body.String())
	}
	// Trash is now empty.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-hosted/trash", nil))
	json.NewDecoder(rw.Body).Decode(&trashResp)
	if len(trashResp.Trash) != 0 {
		t.Errorf("trash after purge: %d tombstones, want 0", len(trashResp.Trash))
	}
}
