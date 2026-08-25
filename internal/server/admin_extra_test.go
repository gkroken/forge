package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forge/internal/obs"
	"forge/internal/repo"
)

// mkProxyRepo adds a proxy repo to the server's manager for cache/health tests.
func mkProxyRepo(t *testing.T, srv *Server, name, upstream string) {
	t.Helper()
	if err := srv.Repos.Add(repo.Repository{
		Name: name, Format: "npm", Kind: repo.Proxy, Upstream: upstream, AnonymousRead: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCacheStats_ProxyOnly(t *testing.T) {
	srv := newAdminServer(t)
	mkProxyRepo(t, srv, "npm-proxy", "https://registry.npmjs.org")
	srv.Repos.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted}) //nolint:errcheck
	h := srv.Routes()

	// Proxy → 200 with a 24-bucket ring.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-proxy/cache-stats", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("proxy cache-stats: status %d (%s)", rw.Code, rw.Body.String())
	}
	var snap obs.StatsSnapshot
	json.NewDecoder(rw.Body).Decode(&snap)
	if len(snap.Hourly) != 24 {
		t.Errorf("expected 24 hourly buckets, got %d", len(snap.Hourly))
	}

	// Hosted → 400 (not a proxy).
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-hosted/cache-stats", nil))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("hosted cache-stats: status %d, want 400", rw.Code)
	}

	// Missing → 404.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/ghost/cache-stats", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("missing cache-stats: status %d, want 404", rw.Code)
	}
}

func TestInvalidate_DeletesCachedBlobs(t *testing.T) {
	srv := newAdminServer(t)
	mkProxyRepo(t, srv, "npm-proxy", "https://registry.npmjs.org")

	// Seed two cache entries (blob + meta) plus the health record that must survive.
	cacheNS := "npm-proxy:proxy"
	srv.Blob.Put("npm-proxy/a", strings.NewReader("aaa"))      //nolint:errcheck
	srv.Blob.Put("npm-proxy/b", strings.NewReader("bbb"))      //nolint:errcheck
	srv.Meta.PutJSON(cacheNS, "npm-proxy/a", map[string]int{}) //nolint:errcheck
	srv.Meta.PutJSON(cacheNS, "npm-proxy/b", map[string]int{}) //nolint:errcheck

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/npm-proxy/invalidate", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("invalidate: status %d (%s)", rw.Code, rw.Body.String())
	}
	var resp map[string]int
	json.NewDecoder(rw.Body).Decode(&resp)
	if resp["deleted"] != 2 {
		t.Errorf("deleted = %d, want 2", resp["deleted"])
	}
	if _, ok, _ := srv.Blob.Stat("npm-proxy/a"); ok {
		t.Error("cached blob a should have been deleted")
	}
}

func TestRepoHealth_State(t *testing.T) {
	srv := newAdminServer(t)
	mkProxyRepo(t, srv, "npm-proxy", "https://never-contacted.invalid")

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-proxy/health", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("repo health: status %d", rw.Code)
	}
	var resp map[string]string
	json.NewDecoder(rw.Body).Decode(&resp)
	// Never contacted → breaker closed → "Closed".
	if resp["state"] != "Closed" {
		t.Errorf("state = %q, want Closed", resp["state"])
	}
}

func TestExpireCache_SingleKey(t *testing.T) {
	srv := newAdminServer(t)
	mkProxyRepo(t, srv, "npm-proxy", "https://registry.npmjs.org")
	srv.Blob.Put("npm-proxy/pkg/-/pkg-1.0.0.tgz", strings.NewReader("data")) //nolint:errcheck

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodDelete,
		"/api/v1/repos/npm-proxy/cache/pkg/-/pkg-1.0.0.tgz", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("expire cache: status %d (%s)", rw.Code, rw.Body.String())
	}
	if _, ok, _ := srv.Blob.Stat("npm-proxy/pkg/-/pkg-1.0.0.tgz"); ok {
		t.Error("expired cache blob should be gone")
	}
}

func TestBlobStores_List(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/blob-stores", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("blob-stores: status %d", rw.Code)
	}
	var stores []map[string]string
	json.NewDecoder(rw.Body).Decode(&stores)
	if len(stores) != 1 || stores[0]["name"] != "default" {
		t.Errorf("got %+v, want [{name:default}]", stores)
	}
}

func TestAuditAPI_FiltersAndInitials(t *testing.T) {
	srv := newAdminServer(t)
	al := obs.NewAuditLog(50)
	al.Append(obs.AuditEntry{Timestamp: time.Now(), Actor: "Alice Smith", Method: "PUT", Path: "/repository/npm-hosted/pkg", Status: 201})
	al.Append(obs.AuditEntry{Timestamp: time.Now(), Actor: "bob", Method: "DELETE", Path: "/repository/maven-hosted/x", Status: 403})
	srv.WithAuditLog(al)
	h := srv.Routes()

	// Unfiltered → both entries, with computed initials.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("audit: status %d", rw.Code)
	}
	var entries []struct {
		Actor    string `json:"actor"`
		Initials string `json:"initials"`
		Path     string `json:"path"`
		OK       bool   `json:"ok"`
	}
	json.NewDecoder(rw.Body).Decode(&entries)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	var aliceInitials string
	for _, e := range entries {
		if e.Actor == "Alice Smith" {
			aliceInitials = e.Initials
		}
	}
	if aliceInitials != "AS" {
		t.Errorf("Alice Smith initials = %q, want AS", aliceInitials)
	}

	// Filter by repo → only the npm-hosted entry.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/audit?repo=npm-hosted", nil))
	json.NewDecoder(rw.Body).Decode(&entries)
	if len(entries) != 1 || !strings.Contains(entries[0].Path, "npm-hosted") {
		t.Errorf("repo filter: got %+v, want single npm-hosted entry", entries)
	}

	// Filter by actor.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/audit?actor=bob", nil))
	json.NewDecoder(rw.Body).Decode(&entries)
	if len(entries) != 1 || entries[0].Actor != "bob" {
		t.Errorf("actor filter: got %+v, want single bob entry", entries)
	}
}

func TestDeleteComponent_Validation(t *testing.T) {
	srv := newAdminServer(t)
	srv.Repos.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted})                      //nolint:errcheck
	srv.Repos.Add(repo.Repository{Name: "immutable-repo", Format: "npm", Kind: repo.Hosted, Immutable: true}) //nolint:errcheck
	h := srv.Routes()

	// Missing name/version → 400.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodDelete, "/api/v1/repos/npm-hosted/component", nil))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("missing params: status %d, want 400", rw.Code)
	}

	// Immutable repo → 409.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodDelete,
		"/api/v1/repos/immutable-repo/component?name=x&version=1.0.0", nil))
	if rw.Code != http.StatusConflict {
		t.Errorf("immutable delete: status %d, want 409", rw.Code)
	}

	// Missing repo → 404.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodDelete,
		"/api/v1/repos/ghost/component?name=x&version=1.0.0", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("missing repo: status %d, want 404", rw.Code)
	}
}
