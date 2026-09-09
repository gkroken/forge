package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/auth"
	"forge/internal/repo"
)

// RequireRepoRead gates the browse and component-listing endpoints, which
// report what a repository holds without serving its bytes. They previously had
// no check at all, so a private repository's package names and versions were
// readable by anyone — the reconnaissance half of a dependency-confusion attack.
//
// It has to accept both credential shapes those endpoints actually see: a
// Bearer token from API clients, and the UI session cookie from a signed-in
// browser.
func TestRequireRepoRead(t *testing.T) {
	store := newStore(t)
	mgr := repo.NewManager()
	if err := mgr.Add(repo.Repository{Name: "private", Format: "npm", Kind: repo.Hosted, AnonymousRead: false}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Add(repo.Repository{Name: "public", Format: "npm", Kind: repo.Hosted, AnonymousRead: true}); err != nil {
		t.Fatal(err)
	}
	enforcer := auth.NewEnforcer(store, mgr)

	_, readSecret, err := store.Create("reader", []auth.Grant{
		{Repo: "private", Actions: []auth.Action{auth.ActionRead}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, otherSecret, err := store.Create("other-repo-only", []auth.Grant{
		{Repo: "somewhere-else", Actions: []auth.Action{auth.ActionRead}}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	call := func(repoName string, apply func(*http.Request)) int {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+repoName+"/components", nil)
		if apply != nil {
			apply(r)
		}
		w := httptest.NewRecorder()
		if enforcer.RequireRepoRead(w, r, repoName) {
			return http.StatusOK
		}
		return w.Code
	}
	bearer := func(s string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+s) }
	}
	cookie := func(s string) func(*http.Request) {
		return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.UISessionCookie, Value: s}) }
	}

	for _, tc := range []struct {
		desc, repo string
		apply      func(*http.Request)
		want       int
	}{
		{"anonymous on a private repo", "private", nil, http.StatusUnauthorized},
		{"anonymous on a public repo", "public", nil, http.StatusOK},
		{"read grant on a private repo", "private", bearer(readSecret), http.StatusOK},
		{"grant for a different repo", "private", bearer(otherSecret), http.StatusForbidden},
		{"invalid token", "private", bearer("forge_not-a-real-token"), http.StatusUnauthorized},
		{"UI session cookie", "private", cookie(readSecret), http.StatusOK},
		{"cookie for a different repo", "private", cookie(otherSecret), http.StatusForbidden},
		{"unknown repo falls through to the handler", "no-such-repo", nil, http.StatusOK},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if got := call(tc.repo, tc.apply); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// With auth disabled entirely, browse must stay open — eval mode is AllowAll.
func TestRequireRepoRead_EvalMode(t *testing.T) {
	enforcer := auth.NewEnforcer(nil, repo.NewManager())
	w := httptest.NewRecorder()
	if !enforcer.RequireRepoRead(w, httptest.NewRequest(http.MethodGet, "/x", nil), "anything") {
		t.Errorf("eval mode denied a read (%d); auth is not configured", w.Code)
	}
}

// A Bearer header must win over a stale cookie rather than being masked by it.
func TestRequireRepoRead_BearerBeatsCookie(t *testing.T) {
	store := newStore(t)
	mgr := repo.NewManager()
	if err := mgr.Add(repo.Repository{Name: "private", Format: "npm", Kind: repo.Hosted}); err != nil {
		t.Fatal(err)
	}
	enforcer := auth.NewEnforcer(store, mgr)
	_, good, err := store.Create("good", []auth.Grant{
		{Repo: "private", Actions: []auth.Action{auth.ActionRead}}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Authorization", "Bearer "+good)
	r.AddCookie(&http.Cookie{Name: auth.UISessionCookie, Value: "forge_stale-cookie"})
	w := httptest.NewRecorder()
	if !enforcer.RequireRepoRead(w, r, "private") {
		t.Errorf("valid Bearer rejected because a stale cookie was present (%d)", w.Code)
	}
}
