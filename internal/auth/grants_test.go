package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/auth"
	"forge/internal/repo"
)

// TestGrant_LegacyUnmarshal guards the pre-actions wire/persistence shape:
// tokens stored as {"repo":"x","role":N} must expand to the equivalent
// action bundle so existing tokens keep working after the schema change.
func TestGrant_LegacyUnmarshal(t *testing.T) {
	cases := []struct {
		json string
		want []auth.Action
	}{
		{`{"repo":"x","role":1}`, []auth.Action{auth.ActionRead}},
		{`{"repo":"x","role":2}`, []auth.Action{auth.ActionRead, auth.ActionWrite, auth.ActionDelete}},
		{`{"repo":"x","role":3}`, []auth.Action{auth.ActionRead, auth.ActionWrite, auth.ActionDelete, auth.ActionAdmin}},
		// New shape wins when both present.
		{`{"repo":"x","role":3,"actions":["read"]}`, []auth.Action{auth.ActionRead}},
		// New shape alone.
		{`{"repo":"x","actions":["read","write"]}`, []auth.Action{auth.ActionRead, auth.ActionWrite}},
	}
	for _, c := range cases {
		var g auth.Grant
		if err := json.Unmarshal([]byte(c.json), &g); err != nil {
			t.Fatalf("unmarshal %s: %v", c.json, err)
		}
		if len(g.Actions) != len(c.want) {
			t.Fatalf("%s: got %v want %v", c.json, g.Actions, c.want)
		}
		for i := range c.want {
			if g.Actions[i] != c.want[i] {
				t.Errorf("%s: got %v want %v", c.json, g.Actions, c.want)
			}
		}
	}

	// Selectors round-trip through the new shape.
	var g auth.Grant
	err := json.Unmarshal([]byte(`{"repo":"x","actions":["read"],"selectors":["com/acme/**"]}`), &g)
	if err != nil || len(g.Selectors) != 1 || g.Selectors[0] != "com/acme/**" {
		t.Fatalf("selectors: got %+v err=%v", g, err)
	}
}

func TestValidateGrants(t *testing.T) {
	valid := [][]auth.Grant{
		{{Repo: "*", Actions: []auth.Action{auth.ActionRead}}},
		{{Repo: "x", Actions: []auth.Action{auth.ActionRead, auth.ActionWrite}, Selectors: []string{"com/acme/**"}}},
		{auth.GrantForRole("x", auth.RoleAdmin)},
	}
	for i, g := range valid {
		if err := auth.ValidateGrants(g); err != nil {
			t.Errorf("valid[%d]: unexpected error %v", i, err)
		}
	}

	invalid := [][]auth.Grant{
		nil, // empty
		{{Repo: "", Actions: []auth.Action{auth.ActionRead}}}, // no repo
		{{Repo: "x"}}, // no actions
		{{Repo: "x", Actions: []auth.Action{"push"}}},                                                 // unknown action
		{{Repo: "x", Actions: []auth.Action{auth.ActionAdmin}, Selectors: []string{"com/**"}}},        // admin + selector
		{{Repo: "x", Actions: []auth.Action{auth.ActionRead}, Selectors: []string{"/leading-slash"}}}, // bad selector
	}
	for i, g := range invalid {
		if err := auth.ValidateGrants(g); err == nil {
			t.Errorf("invalid[%d]: expected error, got nil", i)
		}
	}
}

// TestAuthzMatrix_Selectors exercises selector-scoped grants end-to-end
// through the middleware: the grant only covers paths matching its patterns.
func TestAuthzMatrix_Selectors(t *testing.T) {
	store := newStore(t)
	mgr := repo.NewManager()
	mgr.Add(repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted})
	mgr.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted})
	enforcer := auth.NewEnforcer(store, mgr)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux := http.NewServeMux()
	mux.Handle("/repository/", enforcer.Middleware(inner))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// CI token: may publish only under com/acme in maven-hosted and only
	// @acme-scoped packages in npm-hosted; may read everything in both.
	_, secret, err := store.Create("scoped ci", []auth.Grant{
		{Repo: "maven-hosted", Actions: []auth.Action{auth.ActionRead}},
		{Repo: "maven-hosted", Actions: []auth.Action{auth.ActionWrite}, Selectors: []string{"com/acme/**"}},
		{Repo: "npm-hosted", Actions: []auth.Action{auth.ActionRead}},
		{Repo: "npm-hosted", Actions: []auth.Action{auth.ActionWrite}, Selectors: []string{"@acme/**"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, method, path string
		want               int
	}{
		{"read anywhere", "GET", "/repository/maven-hosted/org/other/lib/1.0/lib-1.0.jar", http.StatusOK},
		{"write inside selector", "PUT", "/repository/maven-hosted/com/acme/app/1.0/app-1.0.jar", http.StatusOK},
		{"write outside selector", "PUT", "/repository/maven-hosted/org/other/lib/1.0/lib-1.0.jar", http.StatusForbidden},
		{"prefix must be segment-exact", "PUT", "/repository/maven-hosted/com/acmeco/app/1.0/a.jar", http.StatusForbidden},
		{"npm publish own scope", "PUT", "/repository/npm-hosted/@acme/ui", http.StatusOK},
		{"npm publish foreign scope", "PUT", "/repository/npm-hosted/@other/ui", http.StatusForbidden},
		{"npm publish unscoped", "PUT", "/repository/npm-hosted/lodash", http.StatusForbidden},
		{"delete not granted at all", "DELETE", "/repository/maven-hosted/com/acme/app/1.0/app-1.0.jar", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := http.NewRequest(c.method, srv.URL+c.path, nil)
			r.Header.Set("Authorization", "Bearer "+secret)
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Errorf("%s %s: got %d want %d", c.method, c.path, resp.StatusCode, c.want)
			}
		})
	}
}

// TestRequireRepoAdmin verifies the repo-scoped admin guard: a repo-admin
// grant unlocks its own repo, not others; global admin unlocks everything;
// content-only grants never pass.
func TestRequireRepoAdmin(t *testing.T) {
	store := newStore(t)
	enforcer := auth.NewEnforcer(store, repo.NewManager())

	_, repoAdmin, _ := store.Create("repo admin", []auth.Grant{
		{Repo: "npm-hosted", Actions: []auth.Action{auth.ActionAdmin}},
	}, nil)
	_, globalAdmin, _ := store.Create("global admin", []auth.Grant{auth.GrantForRole("*", auth.RoleAdmin)}, nil)
	_, writer, _ := store.Create("writer", []auth.Grant{auth.GrantForRole("npm-hosted", auth.RoleWrite)}, nil)

	check := func(secret, repoName string) int {
		r := httptest.NewRequest("PUT", "/api/v1/repos/"+repoName, nil)
		if secret != "" {
			r.Header.Set("Authorization", "Bearer "+secret)
		}
		w := httptest.NewRecorder()
		if enforcer.RequireRepoAdmin(w, r, repoName) {
			return http.StatusOK
		}
		return w.Code
	}

	cases := []struct {
		name, secret, repo string
		want               int
	}{
		{"repo admin on own repo", repoAdmin, "npm-hosted", http.StatusOK},
		{"repo admin on other repo", repoAdmin, "maven-hosted", http.StatusForbidden},
		{"global admin on any repo", globalAdmin, "maven-hosted", http.StatusOK},
		{"content writer is not admin", writer, "npm-hosted", http.StatusForbidden},
		{"anonymous", "", "npm-hosted", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := check(c.secret, c.repo); got != c.want {
				t.Errorf("got %d want %d", got, c.want)
			}
		})
	}

	// A repo-scoped admin must NOT pass the global-admin guard.
	r := httptest.NewRequest("GET", "/api/v1/tokens", nil)
	r.Header.Set("Authorization", "Bearer "+repoAdmin)
	w := httptest.NewRecorder()
	if enforcer.RequireAdmin(w, r) {
		t.Error("repo-scoped admin passed the global RequireAdmin guard")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}
