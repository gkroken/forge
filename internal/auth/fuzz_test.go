package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/auth"
	"forge/internal/meta"
	"forge/internal/repo"
)

// FuzzEnforcerDecide drives the auth enforcement layer with arbitrary
// Authorization header values and repository paths. The invariant is that
// no input combination causes a panic — every request must produce a valid
// HTTP response (200, 401, or 403).
func FuzzEnforcerDecide(f *testing.F) {
	f.Add("", "npm-hosted")
	f.Add("Bearer forge_"+strings.Repeat("a", 64), "npm-hosted")
	f.Add("Bearer forge_"+strings.Repeat("0", 64), "private")
	f.Add("Basic dXNlcjpwYXNz", "npm-hosted")        // standard Basic auth
	f.Add("Bearer \x00null-byte", "npm-hosted")
	f.Add(strings.Repeat("x", 4096), "npm-hosted")   // very long value
	f.Add("forge_notprefixedwithbearer", "npm-hosted")
	f.Add("Bearer forge_short", "unknown-repo")

	m, _ := meta.NewFS(f.TempDir())
	store := auth.NewMetaStore(m)

	mgr := repo.NewManager()
	mgr.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, AnonymousRead: true})
	mgr.Add(repo.Repository{Name: "private", Format: "npm", Kind: repo.Hosted, AnonymousRead: false})

	enforcer := auth.NewEnforcer(store, mgr)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := enforcer.Middleware(inner)

	f.Fuzz(func(t *testing.T, authHeader, repoName string) {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			req := httptest.NewRequest(method, "/repository/"+repoName+"/artifact", nil)
			if authHeader != "" {
				req.Header.Set("Authorization", authHeader)
			}
			rw := httptest.NewRecorder()
			handler.ServeHTTP(rw, req)
			status := rw.Code
			if status != http.StatusOK && status != http.StatusUnauthorized && status != http.StatusForbidden {
				t.Errorf("unexpected status %d for method=%s repo=%q auth=%q",
					status, method, repoName, authHeader)
			}
		}
	})
}
