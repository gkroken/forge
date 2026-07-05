package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/repo"
)

// TestReindex_NoopForGeneratedIndex covers handleReindex's honest "noop" branch
// for a format whose indexes are generated on demand (maven has no Reindexer).
func TestReindex_NoopForGeneratedIndex(t *testing.T) {
	srv := newMigrationServer(t)
	addHosted(t, srv, "mvn-hosted", "maven")
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/mvn-hosted/reindex", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("maven reindex: status %d (%s)", rw.Code, rw.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(rw.Body).Decode(&resp)
	if resp["status"] != "noop" {
		t.Errorf("status = %q, want noop", resp["status"])
	}
}

// TestTopLevelAPI_MethodBranches covers the default/method-not-allowed branches
// of the top-level admin API dispatchers.
func TestTopLevelAPI_MethodBranches(t *testing.T) {
	srv := newUserMgmtServer(t) // Users + Roles wired
	srv.Repos.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted}) //nolint:errcheck
	h := srv.Routes()

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodPut, "/api/v1/users", http.StatusNotFound},           // handleUsers default
		{http.MethodPatch, "/api/v1/roles", http.StatusNotFound},         // handleRoles default
		{http.MethodPost, "/api/v1/audit", http.StatusMethodNotAllowed},  // handleAuditAPI GET-only
		{http.MethodPut, "/api/v1/blob-stores", http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, adminReq(t, c.method, c.path, nil))
		if rw.Code != c.want {
			t.Errorf("%s %s: status %d, want %d", c.method, c.path, rw.Code, c.want)
		}
	}
}
