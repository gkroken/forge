package server_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/formats"
	"forge/internal/meta"
	"forge/internal/repo"
	"forge/internal/server"
)

type authEnv struct {
	srv        http.Handler
	repo       string
	publicRepo string
	readToken  string
}

// newAuthEnv builds a server with auth on, one private repository and one
// public one.
func newAuthEnv(t *testing.T) authEnv {
	t.Helper()
	dir := t.TempDir()
	b, err := blob.NewFS(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewFS(filepath.Join(dir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "private-npm", Format: "npm", Kind: repo.Hosted, Enabled: true, AnonymousRead: false},
		{Name: "public-npm", Format: "npm", Kind: repo.Hosted, Enabled: true, AnonymousRead: true},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	store := auth.NewMetaStore(m)
	_, secret, err := store.Create("reader", []auth.Grant{
		{Repo: "private-npm", Actions: []auth.Action{auth.ActionRead}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(mgr, formats.Registry(), b, m, store)
	return authEnv{srv: s.Routes(), repo: "private-npm", publicRepo: "public-npm", readToken: secret}
}

// The browse endpoints report what a repository holds. They are not artifact
// downloads, which is why they were exempted from the admin check — but they
// were exempted from EVERY check, so an unauthenticated caller could list every
// package name and version in a private repository.
//
// Those names are exactly the target list for a dependency-confusion attack:
// knowing "@acme/toolkit" exists internally is what tells an attacker which
// name to register publicly. Group shadowing defends the pull; this defends the
// reconnaissance.
func TestBrowseEndpointsRequireReadPermission(t *testing.T) {
	env := newAuthEnv(t) // private repo (anonymousRead=false) + a read token
	for _, path := range []string{
		"/api/v1/repos/" + env.repo + "/components",
		"/ui/browse/" + env.repo + "/tree",
		"/ui/browse/" + env.repo + "/versions?pkg=x",
		"/ui/browse/" + env.repo + "/detail?pkg=x&ver=1.0.0",
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			env.srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("unauthenticated GET %s = %d, want 401 — a private repo's "+
					"contents must not be listable anonymously", path, w.Code)
			}

			w = httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", "Bearer "+env.readToken)
			env.srv.ServeHTTP(w, r)
			if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
				t.Errorf("authorised GET %s = %d, want it allowed", path, w.Code)
			}
		})
	}
}

// A repository that IS public stays browsable without credentials.
func TestBrowsePublicRepoStaysAnonymous(t *testing.T) {
	env := newAuthEnv(t)
	w := httptest.NewRecorder()
	env.srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/repos/"+env.publicRepo+"/components", nil))
	if w.Code != http.StatusOK {
		t.Errorf("anonymous browse of a public repo = %d, want 200", w.Code)
	}
}
