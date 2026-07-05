package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/auth"
	"forge/internal/repo"
)

// TestRepoAdmin_Scoping verifies that per-repo admin API routes honour a
// repo-scoped admin grant while system-level routes still require global
// admin, end-to-end through Routes().
func TestRepoAdmin_Scoping(t *testing.T) {
	srv, authStore := newAuthServer(t)
	srv.Repos.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, Enabled: true})
	srv.Repos.Add(repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted, Enabled: true})

	_, repoAdmin, err := authStore.Create("npm repo admin", []auth.Grant{
		{Repo: "npm-hosted", Actions: []auth.Action{auth.ActionAdmin}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, globalAdmin, _ := authStore.Create("global admin", []auth.Grant{auth.GrantForRole("*", auth.RoleAdmin)}, nil)

	do := func(method, path, bearer, body string) int {
		rw := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rw, tokenReq(t, method, path, bearer, body))
		return rw.Code
	}

	repoBody := `{"name":"npm-hosted","format":"npm","kind":"hosted","enabled":true}`

	cases := []struct {
		name, method, path, bearer, body string
		want                             int
	}{
		// Own repo: settings, access view, health all reachable.
		{"get own repo", "GET", "/api/v1/repos/npm-hosted", repoAdmin, "", http.StatusOK},
		{"update own repo", "PUT", "/api/v1/repos/npm-hosted", repoAdmin, repoBody, http.StatusOK},
		{"access of own repo", "GET", "/api/v1/repos/npm-hosted/access", repoAdmin, "", http.StatusOK},
		{"verify report of own repo", "GET", "/api/v1/repos/npm-hosted/verify", repoAdmin, "", http.StatusOK},
		{"trash list of own repo", "GET", "/api/v1/repos/npm-hosted/trash", repoAdmin, "", http.StatusOK},

		// Another repo: denied.
		{"get other repo", "GET", "/api/v1/repos/maven-hosted", repoAdmin, "", http.StatusForbidden},
		{"cleanup other repo", "POST", "/api/v1/repos/maven-hosted/cleanup", repoAdmin, "", http.StatusForbidden},
		{"verify other repo", "POST", "/api/v1/repos/maven-hosted/verify", repoAdmin, "", http.StatusForbidden},
		{"trash of other repo", "GET", "/api/v1/repos/maven-hosted/trash", repoAdmin, "", http.StatusForbidden},

		// System-level routes: repo admin is not enough.
		{"list repos", "GET", "/api/v1/repos", repoAdmin, "", http.StatusForbidden},
		{"create repo", "POST", "/api/v1/repos", repoAdmin, repoBody, http.StatusForbidden},
		{"list tokens", "GET", "/api/v1/tokens", repoAdmin, "", http.StatusForbidden},
		{"integrity rollup", "GET", "/api/v1/integrity", repoAdmin, "", http.StatusForbidden},

		// Global admin passes everywhere.
		{"global admin lists repos", "GET", "/api/v1/repos", globalAdmin, "", http.StatusOK},
		{"global admin other repo", "GET", "/api/v1/repos/maven-hosted", globalAdmin, "", http.StatusOK},
		{"global admin integrity rollup", "GET", "/api/v1/integrity", globalAdmin, "", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := do(c.method, c.path, c.bearer, c.body); got != c.want {
				t.Errorf("%s %s: got %d want %d", c.method, c.path, got, c.want)
			}
		})
	}
}
