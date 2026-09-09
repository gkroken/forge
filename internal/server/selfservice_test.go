package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/auth"
)

// Self-service exists so a developer does not need an admin to run
// `npm install` against a private registry — the previous arrangement made
// people share tokens instead.
//
// The rule that keeps it safe is that a token can never widen anyone's
// authority: you may only mint what your own credential already carries.
func TestSelfService_CannotExceedYourOwnAuthority(t *testing.T) {
	srv, store := newAuthServer(t)
	_, _, _ = store.Create("admin", []auth.Grant{auth.GrantForRole("*", auth.RoleAdmin)}, nil)
	// bob may read and write acme-npm, and nothing else.
	_, bob, _ := store.Create("bob session", []auth.Grant{
		{Repo: "acme-npm", Actions: []auth.Action{auth.ActionRead, auth.ActionWrite}},
	}, nil, "bob")

	mint := func(grants string) (int, string) {
		rw := httptest.NewRecorder()
		body := `{"description":"ci","grants":` + grants + `}`
		srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodPost, "/api/v1/tokens", bob, body))
		return rw.Code, rw.Body.String()
	}

	t.Run("within your grants is allowed", func(t *testing.T) {
		code, body := mint(`[{"repo":"acme-npm","actions":["read"]}]`)
		if code != http.StatusCreated {
			t.Fatalf("got %d: %s", code, body)
		}
		var resp struct {
			Owner  string `json:"owner"`
			Secret string `json:"secret"`
		}
		_ = json.Unmarshal([]byte(body), &resp)
		if resp.Owner != "bob" {
			t.Errorf("owner = %q, want bob — an ownerless token cannot be found or revoked by its user", resp.Owner)
		}
		if resp.Secret == "" {
			t.Error("no secret returned")
		}
	})

	for _, tc := range []struct{ desc, grants string }{
		{"a wider action", `[{"repo":"acme-npm","actions":["admin"]}]`},
		{"a repository you cannot touch", `[{"repo":"other-repo","actions":["read"]}]`},
		{"the wildcard", `[{"repo":"*","actions":["read"]}]`},
		{"delete, which bob does not have", `[{"repo":"acme-npm","actions":["delete"]}]`},
	} {
		t.Run(tc.desc+" is refused", func(t *testing.T) {
			code, body := mint(tc.grants)
			if code != http.StatusForbidden {
				t.Errorf("got %d, want 403 — minting %s would widen bob's authority: %s",
					code, tc.desc, body)
			}
			if !strings.Contains(body, "cannot grant more than you have") {
				t.Errorf("unhelpful refusal: %s", body)
			}
		})
	}
}

// A credential with no owner is not a person, and tokens minted from one could
// not be listed or revoked by anybody but an admin. Those must not self-mint.
func TestSelfService_OwnerlessCredentialCannotMint(t *testing.T) {
	srv, store := newAuthServer(t)
	_, _, _ = store.Create("admin", []auth.Grant{auth.GrantForRole("*", auth.RoleAdmin)}, nil)
	_, ci, _ := store.Create("a CI token, no owner", []auth.Grant{
		{Repo: "acme-npm", Actions: []auth.Action{auth.ActionRead, auth.ActionWrite}},
	}, nil)

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodPost, "/api/v1/tokens", ci,
		`{"description":"chained","grants":[{"repo":"acme-npm","actions":["read"]}]}`))
	if rw.Code != http.StatusForbidden {
		t.Errorf("an ownerless token minted another (%d): tokens would beget tokens "+
			"nobody can find or revoke", rw.Code)
	}
}

// An admin is unrestricted, and keeps seeing everything — which is what makes
// self-service auditable rather than a blind spot.
func TestSelfService_AdminUnaffected(t *testing.T) {
	srv, store := newAuthServer(t)
	_, admin, _ := store.Create("admin", []auth.Grant{auth.GrantForRole("*", auth.RoleAdmin)}, nil)
	usersOwn, _, _ := store.Create("bob's", []auth.Grant{auth.GrantForRole("x", auth.RoleRead)}, nil, "bob")

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodPost, "/api/v1/tokens", admin,
		`{"description":"anything","grants":[{"repo":"*","actions":["admin"]}]}`))
	if rw.Code != http.StatusCreated {
		t.Errorf("admin mint: got %d", rw.Code)
	}
	rw = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodGet, "/api/v1/tokens", admin, ""))
	if !strings.Contains(rw.Body.String(), usersOwn.ID) {
		t.Errorf("an admin cannot see a user's token; self-service would be unauditable")
	}
}

// Config-as-code defines roles in git. Without a way to mint from one, the
// definition could not be used for the credential it exists to describe: every
// CI token restated the grants inline and drifted from the role the moment it
// changed.
func TestCreateToken_FromNamedRole(t *testing.T) {
	srv, store := newAuthServer(t)
	srv = srv.WithRoles(auth.NewRoleStore(srv.Meta))
	_, admin, _ := store.Create("admin", []auth.Grant{auth.GrantForRole("*", auth.RoleAdmin)}, nil)
	if err := srv.Roles.Create(auth.CustomRole{
		Name: "ci-publisher", BaseRole: "read",
		Grants: []auth.Grant{{Repo: "acme-npm", Actions: []auth.Action{auth.ActionRead, auth.ActionWrite}}},
	}); err != nil {
		t.Fatal(err)
	}

	mint := func(body string) (int, string) {
		rw := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodPost, "/api/v1/tokens", admin, body))
		return rw.Code, rw.Body.String()
	}

	t.Run("a config-defined role expands to its grants", func(t *testing.T) {
		code, body := mint(`{"description":"ci","role":"ci-publisher"}`)
		if code != http.StatusCreated {
			t.Fatalf("got %d: %s", code, body)
		}
		if !strings.Contains(body, "acme-npm") || !strings.Contains(body, "write") {
			t.Errorf("token did not carry the role's grants: %s", body)
		}
	})

	t.Run("a predefined role works without a custom one", func(t *testing.T) {
		code, body := mint(`{"description":"r","role":"Reader"}`)
		if code != http.StatusCreated {
			t.Fatalf("got %d: %s", code, body)
		}
		if !strings.Contains(body, "read") {
			t.Errorf("Reader did not expand: %s", body)
		}
	})

	t.Run("an unknown role is refused", func(t *testing.T) {
		code, body := mint(`{"description":"x","role":"no-such-role"}`)
		if code != http.StatusBadRequest || !strings.Contains(body, "unknown role") {
			t.Errorf("got %d: %s", code, body)
		}
	})

	t.Run("role and grants together are ambiguous and refused", func(t *testing.T) {
		code, body := mint(`{"description":"x","role":"ci-publisher","grants":[{"repo":"a","actions":["read"]}]}`)
		if code != http.StatusBadRequest || !strings.Contains(body, "not both") {
			t.Errorf("got %d: %s", code, body)
		}
	})

	// A role cannot be used to exceed the caller's own authority: the
	// self-service check applies to the expanded grants, not the role name.
	t.Run("a role cannot widen a non-admin caller", func(t *testing.T) {
		_, bob, _ := store.Create("bob session", []auth.Grant{
			{Repo: "acme-npm", Actions: []auth.Action{auth.ActionRead}}}, nil, "bob")
		rw := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodPost, "/api/v1/tokens", bob,
			`{"description":"x","role":"Administrator"}`))
		if rw.Code != http.StatusForbidden {
			t.Errorf("a read-only user minted an Administrator token (%d): %s", rw.Code, rw.Body.String())
		}
	})
}
