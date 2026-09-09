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
