package auth_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"forge/internal/auth"
	"forge/internal/meta"
	"forge/internal/repo"
)

// newStore returns a fresh token store backed by a temp directory.
func newStore(t *testing.T) auth.Store {
	t.Helper()
	m, err := meta.NewFS(filepath.Join(t.TempDir(), "meta"))
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewMetaStore(m)
}

// ── Token lifecycle ───────────────────────────────────────────────────────────

func TestTokenStore_CreateAndVerify(t *testing.T) {
	s := newStore(t)

	tok, secret, err := s.Create("ci token", []auth.Grant{
		{Repo: "npm-hosted", Role: auth.RoleWrite},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !isForgeToken(secret) {
		t.Fatalf("unexpected secret format: %q", secret)
	}

	got, err := s.Verify(secret)
	if err != nil || got == nil {
		t.Fatalf("verify: got=nil err=%v", err)
	}
	if got.ID != tok.ID {
		t.Fatalf("id mismatch: %q != %q", got.ID, tok.ID)
	}
	if got.RoleFor("npm-hosted") != auth.RoleWrite {
		t.Fatalf("role: got %v want write", got.RoleFor("npm-hosted"))
	}
}

func TestTokenStore_VerifyWrongSecret(t *testing.T) {
	s := newStore(t)
	s.Create("t", nil, nil)

	got, err := s.Verify("forge_" + "00" + nHex(62))
	if err != nil || got != nil {
		t.Fatalf("expected nil token for wrong secret, got %v err=%v", got, err)
	}
}

func TestTokenStore_VerifyMalformed(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{"", "bad", "forge_xyz", "Bearer forge_abc"} {
		got, err := s.Verify(bad)
		if err != nil || got != nil {
			t.Errorf("malformed %q: expected nil token, got %v err=%v", bad, got, err)
		}
	}
}

func TestTokenStore_Revoke(t *testing.T) {
	s := newStore(t)
	tok, secret, _ := s.Create("temp", nil, nil)

	if err := s.Revoke(tok.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Verify(secret)
	if got != nil {
		t.Fatal("token still valid after revoke")
	}
	// Second revoke must be idempotent.
	if err := s.Revoke(tok.ID); err != nil {
		t.Fatal("second revoke should not error:", err)
	}
}

func TestTokenStore_Expired(t *testing.T) {
	s := newStore(t)
	past := time.Now().Add(-time.Minute)
	_, secret, _ := s.Create("expired", nil, &past)

	got, _ := s.Verify(secret)
	if got != nil {
		t.Fatal("expired token should not verify")
	}
}

func TestTokenStore_List(t *testing.T) {
	s := newStore(t)
	s.Create("a", nil, nil)
	s.Create("b", nil, nil)
	tokens, err := s.List()
	if err != nil || len(tokens) != 2 {
		t.Fatalf("list: len=%d err=%v", len(tokens), err)
	}
}

func TestToken_WildcardGrant(t *testing.T) {
	tok := &auth.Token{Grants: []auth.Grant{{Repo: "*", Role: auth.RoleAdmin}}}
	for _, repo := range []string{"npm-hosted", "maven-hosted", "helm-hosted"} {
		if tok.RoleFor(repo) != auth.RoleAdmin {
			t.Errorf("wildcard: %s got %v want admin", repo, tok.RoleFor(repo))
		}
	}
}

// ── Authz matrix ─────────────────────────────────────────────────────────────

// setupMatrix builds an httptest.Server with auth enforced.
// Repos: "private" (AnonymousRead=false), "public" (AnonymousRead=true).
// Returns the enforcer and a factory for making requests to the server.
func setupMatrix(t *testing.T) (store auth.Store, request func(method, repo, secret string) int) {
	t.Helper()
	store = newStore(t)

	mgr := repo.NewManager()
	mgr.Add(repo.Repository{Name: "private", Format: "maven", Kind: repo.Hosted, AnonymousRead: false})
	mgr.Add(repo.Repository{Name: "public", Format: "maven", Kind: repo.Hosted, AnonymousRead: true})

	enforcer := auth.NewEnforcer(store, mgr)

	// Minimal handler that returns 200 if auth passes.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux := http.NewServeMux()
	mux.Handle("/repository/", enforcer.Middleware(inner))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	request = func(method, repoName, secret string) int {
		req, _ := http.NewRequest(method, srv.URL+"/repository/"+repoName+"/artifact", nil)
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	return
}

func TestAuthzMatrix(t *testing.T) {
	store, req := setupMatrix(t)

	// Mint tokens for each role on "private".
	_, readSecret, _ := store.Create("read", []auth.Grant{{Repo: "private", Role: auth.RoleRead}}, nil)
	_, writeSecret, _ := store.Create("write", []auth.Grant{{Repo: "private", Role: auth.RoleWrite}}, nil)
	_, adminSecret, _ := store.Create("admin", []auth.Grant{{Repo: "*", Role: auth.RoleAdmin}}, nil)
	_, otherSecret, _ := store.Create("other", []auth.Grant{{Repo: "public", Role: auth.RoleWrite}}, nil)

	cases := []struct {
		name   string
		method string
		repo   string
		secret string
		want   int
	}{
		// ── anonymous ───────────────────────────────────────────────────────
		{"anon GET private",  "GET",    "private", "",          http.StatusUnauthorized},
		{"anon PUT private",  "PUT",    "private", "",          http.StatusUnauthorized},
		{"anon GET public",   "GET",    "public",  "",          http.StatusOK},
		{"anon PUT public",   "PUT",    "public",  "",          http.StatusUnauthorized},

		// ── read token ──────────────────────────────────────────────────────
		{"read GET private",  "GET",    "private", readSecret,  http.StatusOK},
		{"read PUT private",  "PUT",    "private", readSecret,  http.StatusForbidden},

		// ── write token ─────────────────────────────────────────────────────
		{"write GET private", "GET",    "private", writeSecret, http.StatusOK},
		{"write PUT private", "PUT",    "private", writeSecret, http.StatusOK},
		{"write PUT other",   "PUT",    "public",  otherSecret, http.StatusOK},
		{"write cross-repo",  "PUT",    "private", otherSecret, http.StatusForbidden},

		// ── admin wildcard ──────────────────────────────────────────────────
		{"admin GET private", "GET",    "private", adminSecret, http.StatusOK},
		{"admin PUT private", "PUT",    "private", adminSecret, http.StatusOK},
		{"admin GET public",  "GET",    "public",  adminSecret, http.StatusOK},

		// ── invalid token ───────────────────────────────────────────────────
		{"invalid token",     "GET",    "private", "forge_" + nHex(64), http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := req(tc.method, tc.repo, tc.secret)
			if got != tc.want {
				t.Errorf("got %d want %d", got, tc.want)
			}
		})
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func isForgeToken(s string) bool {
	return len(s) == 70 && s[:6] == "forge_" // "forge_" + 64 hex chars
}

func nHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}
