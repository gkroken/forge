package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/queue"
	"forge/internal/repo"
)

// TestAdminRepoRoutes_WrongMethod sweeps the wrong-HTTP-method (405) branch of
// the per-repo admin sub-routes, which the happy-path tests skip.
func TestAdminRepoRoutes_WrongMethod(t *testing.T) {
	srv := newMigrationServer(t)
	srv.Repos.Add(repo.Repository{Name: "npm-proxy", Format: "npm", Kind: repo.Proxy, Upstream: "https://x"}) //nolint:errcheck
	srv.Repos.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted})                      //nolint:errcheck
	h := srv.Routes()

	// endpoint → a method that is NOT allowed for it.
	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/repos/npm-proxy/cache-stats"}, // GET only
		{http.MethodGet, "/api/v1/repos/npm-proxy/invalidate"},   // POST only
		{http.MethodPost, "/api/v1/repos/npm-proxy/health"},      // GET only
		{http.MethodGet, "/api/v1/repos/npm-hosted/reindex"},     // POST only
		{http.MethodGet, "/api/v1/repos/npm-hosted/promote"},     // POST only
		{http.MethodGet, "/api/v1/repos/npm-hosted/component"},   // DELETE only
		{http.MethodGet, "/api/v1/repos/npm-proxy/cache/x/y"},    // DELETE only
	}
	for _, c := range cases {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(c.method, c.path, nil))
		if rw.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status %d, want 405", c.method, c.path, rw.Code)
		}
	}
}

// TestReindexAndVerify covers the npm reindex happy path (the Reindexer seam)
// and the integrity verify GET (never-scanned) + POST (enqueue) paths.
func TestReindexAndVerify(t *testing.T) {
	srv := newMigrationServer(t)
	srv.Queue = queue.NewMem(8)
	addHosted(t, srv, "npm-hosted", "npm")
	// Seed a package so reindex has something to rebuild.
	seedBlobBytes(t, srv, http.MethodPut, "npm-hosted", "leftpad", npmPublishBody("leftpad", "1.0.0", []byte("tgz")))
	h := srv.Routes()

	// Reindex (npm implements Reindexer).
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/npm-hosted/reindex", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("reindex: status %d (%s)", rw.Code, rw.Body.String())
	}

	// Verify GET before any scan → "never".
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-hosted/verify", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("verify GET: status %d", rw.Code)
	}

	// Verify POST enqueues an async job.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/npm-hosted/verify", nil))
	if rw.Code >= 400 {
		t.Fatalf("verify POST: status %d (%s)", rw.Code, rw.Body.String())
	}

	// Reindex a missing repo → 404.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/ghost/reindex", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("reindex missing repo: status %d, want 404", rw.Code)
	}
}
