package auth_test

import (
	"encoding/base64"
	"encoding/json"
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

// TestAuthzMatrix_OCI verifies MiddlewareOCI enforces auth on /v2/{name}/...
// routes and returns the OCI-spec JSON error body + WWW-Authenticate header.
func TestAuthzMatrix_OCI(t *testing.T) {
	store, err := meta.NewFS(filepath.Join(t.TempDir(), "meta"))
	if err != nil {
		t.Fatal(err)
	}
	s := auth.NewMetaStore(store)

	mgr := repo.NewManager()
	mgr.Add(repo.Repository{Name: "oci-private", Format: "oci", Kind: repo.Hosted, AnonymousRead: false})
	mgr.Add(repo.Repository{Name: "oci-public", Format: "oci", Kind: repo.Hosted, AnonymousRead: true})

	enforcer := auth.NewEnforcer(s, mgr)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux := http.NewServeMux()
	mux.Handle("/v2/", enforcer.MiddlewareOCI(inner))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, writeSecret, _ := s.Create("write", []auth.Grant{{Repo: "oci-private", Role: auth.RoleWrite}}, nil)
	_, readSecret, _ := s.Create("read", []auth.Grant{{Repo: "oci-private", Role: auth.RoleRead}}, nil)

	doOCI := func(method, repo, secret string) (int, http.Header, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+"/v2/"+repo+"/manifests/latest", nil)
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, resp.Header, body
	}

	cases := []struct {
		name       string
		method     string
		repo       string
		secret     string
		wantStatus int
		wantWWW    bool // expect WWW-Authenticate header on 401
	}{
		{"anon GET private",   "GET", "oci-private", "",           http.StatusUnauthorized, true},
		{"anon PUT private",   "PUT", "oci-private", "",           http.StatusUnauthorized, true},
		{"anon GET public",    "GET", "oci-public",  "",           http.StatusOK,           false},
		{"write GET private",  "GET", "oci-private", writeSecret,  http.StatusOK,           false},
		{"write PUT private",  "PUT", "oci-private", writeSecret,  http.StatusOK,           false},
		{"read PUT private",   "PUT", "oci-private", readSecret,   http.StatusForbidden,    false},
		{"invalid token",      "GET", "oci-private", "forge_" + nHex(64), http.StatusUnauthorized, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, hdr, body := doOCI(tc.method, tc.repo, tc.secret)
			if status != tc.wantStatus {
				t.Errorf("status: got %d want %d", status, tc.wantStatus)
			}
			if tc.wantWWW && hdr.Get("WWW-Authenticate") == "" {
				t.Error("missing WWW-Authenticate header on 401")
			}
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				errs, _ := body["errors"].([]any)
				if len(errs) == 0 {
					t.Error("OCI error response missing 'errors' array")
				}
			}
		})
	}
}

// TestAuthzMatrix_Methods verifies HEAD maps to read and DELETE/POST/PATCH
// map to write, consistent with actionFor() in enforce.go.
func TestAuthzMatrix_Methods(t *testing.T) {
	store, req := setupMatrix(t)
	_, readSecret, _ := store.Create("read", []auth.Grant{{Repo: "private", Role: auth.RoleRead}}, nil)
	_, writeSecret, _ := store.Create("write", []auth.Grant{{Repo: "private", Role: auth.RoleWrite}}, nil)

	cases := []struct {
		name   string
		method string
		secret string
		want   int
	}{
		// HEAD is a read — read token allows, anon denied.
		{"HEAD anon",        "HEAD",   "",          http.StatusUnauthorized},
		{"HEAD read-token",  "HEAD",   readSecret,  http.StatusOK},
		// DELETE, POST, PATCH are writes.
		{"DELETE read-token",  "DELETE", readSecret,  http.StatusForbidden},
		{"DELETE write-token", "DELETE", writeSecret, http.StatusOK},
		{"POST read-token",    "POST",   readSecret,  http.StatusForbidden},
		{"POST write-token",   "POST",   writeSecret, http.StatusOK},
		{"PATCH write-token",  "PATCH",  writeSecret, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := req(tc.method, "private", tc.secret)
			if got != tc.want {
				t.Errorf("got %d want %d", got, tc.want)
			}
		})
	}
}

// TestAuthzMatrix_BearerFormats verifies that npm-style Basic auth (empty
// username, token as password) is accepted alongside the standard Bearer header.
func TestAuthzMatrix_BearerFormats(t *testing.T) {
	m, _ := meta.NewFS(filepath.Join(t.TempDir(), "meta"))
	s := auth.NewMetaStore(m)
	_, secret, _ := s.Create("rw", []auth.Grant{{Repo: "private", Role: auth.RoleWrite}}, nil)

	mgr := repo.NewManager()
	mgr.Add(repo.Repository{Name: "private", Format: "maven", Kind: repo.Hosted, AnonymousRead: false})
	enforcer := auth.NewEnforcer(s, mgr)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux := http.NewServeMux()
	mux.Handle("/repository/", enforcer.Middleware(inner))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	send := func(name, authHeader string, wantStatus int) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			r, _ := http.NewRequest("PUT", srv.URL+"/repository/private/artifact", nil)
			r.Header.Set("Authorization", authHeader)
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != wantStatus {
				t.Errorf("got %d want %d", resp.StatusCode, wantStatus)
			}
		})
	}

	basicEncode := func(token string) string {
		// npm sends Authorization: Basic base64(":"  + token) — empty username.
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+token))
	}

	send("npm Basic auth",         basicEncode(secret),          http.StatusOK)
	send("standard Bearer",        "Bearer "+secret,             http.StatusOK)
	send("Bearer with extra space","Bearer  "+secret,            http.StatusOK)
	send("wrong token via Basic",  basicEncode("forge_"+nHex(64)), http.StatusUnauthorized)
	send("wrong Bearer",           "Bearer forge_"+nHex(64),     http.StatusUnauthorized)
}

// TestAuthzMatrix_RequireAdmin verifies the admin guard on token-management
// endpoints: no token → 401, non-admin → 403, admin → passes through.
func TestAuthzMatrix_RequireAdmin(t *testing.T) {
	s := newStore(t)
	mgr := repo.NewManager()
	enforcer := auth.NewEnforcer(s, mgr)

	_, nonAdminSecret, _ := s.Create("user", []auth.Grant{{Repo: "npm-hosted", Role: auth.RoleWrite}}, nil)
	_, adminSecret, _ := s.Create("admin", []auth.Grant{{Repo: "*", Role: auth.RoleAdmin}}, nil)

	adminHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !enforcer.RequireAdmin(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(adminHandler)
	t.Cleanup(srv.Close)

	do := func(secret string) int {
		r, _ := http.NewRequest("POST", srv.URL+"/api/v1/tokens", nil)
		if secret != "" {
			r.Header.Set("Authorization", "Bearer "+secret)
		}
		resp, _ := http.DefaultClient.Do(r)
		resp.Body.Close()
		return resp.StatusCode
	}

	cases := []struct {
		name   string
		secret string
		want   int
	}{
		{"no token",     "",             http.StatusUnauthorized},
		{"non-admin",    nonAdminSecret, http.StatusForbidden},
		{"admin",        adminSecret,    http.StatusOK},
		{"invalid",      "forge_" + nHex(64), http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := do(tc.secret)
			if got != tc.want {
				t.Errorf("got %d want %d", got, tc.want)
			}
		})
	}
}

// TestAuthzMatrix_ExpiredToken verifies that an expired token is rejected at
// the HTTP layer with 401, not silently treated as anonymous.
func TestAuthzMatrix_ExpiredToken(t *testing.T) {
	_, req := setupMatrix(t)

	// Create a second store to issue an already-expired token.
	s2 := newStore(t)
	past := time.Now().Add(-time.Second)
	_, expiredSecret, _ := s2.Create("expired", []auth.Grant{{Repo: "private", Role: auth.RoleWrite}}, &past)

	// The matrix server uses its own store, so this token is unknown → 401,
	// same as an expired token from the correct store.
	got := req("GET", "private", expiredSecret)
	if got != http.StatusUnauthorized {
		t.Errorf("expired/unknown token: got %d want 401", got)
	}

	// Verify with the correct store: expired → nil from Verify → 401.
	s3 := newStore(t)
	mgr3 := repo.NewManager()
	mgr3.Add(repo.Repository{Name: "private", Format: "maven", Kind: repo.Hosted})
	enforcer3 := auth.NewEnforcer(s3, mgr3)

	pastTime := time.Now().Add(-time.Second)
	_, expSecret, _ := s3.Create("exp", []auth.Grant{{Repo: "private", Role: auth.RoleRead}}, &pastTime)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux := http.NewServeMux()
	mux.Handle("/repository/", enforcer3.Middleware(inner))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r, _ := http.NewRequest("GET", srv.URL+"/repository/private/artifact", nil)
	r.Header.Set("Authorization", "Bearer "+expSecret)
	resp, _ := http.DefaultClient.Do(r)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired token: got %d want 401", resp.StatusCode)
	}
}

func TestRole_String(t *testing.T) {
	cases := []struct {
		role auth.Role
		want string
	}{
		{auth.RoleRead, "read"},
		{auth.RoleWrite, "write"},
		{auth.RoleAdmin, "admin"},
		{auth.RoleNone, "none"},
		{auth.Role(99), "none"},
	}
	for _, tc := range cases {
		if got := tc.role.String(); got != tc.want {
			t.Errorf("Role(%d).String() = %q, want %q", tc.role, got, tc.want)
		}
	}
}

func TestParseRole(t *testing.T) {
	cases := []struct {
		input   string
		want    auth.Role
		wantErr bool
	}{
		{"read", auth.RoleRead, false},
		{"write", auth.RoleWrite, false},
		{"admin", auth.RoleAdmin, false},
		{"", auth.RoleNone, true},
		{"superuser", auth.RoleNone, true},
	}
	for _, tc := range cases {
		got, err := auth.ParseRole(tc.input)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseRole(%q) err=%v wantErr=%v", tc.input, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("ParseRole(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestTokenStore_Count(t *testing.T) {
	s := newStore(t)
	n, err := s.Count()
	if err != nil || n != 0 {
		t.Fatalf("empty store: Count()=%d err=%v", n, err)
	}
	s.Create("t1", nil, nil)
	s.Create("t2", nil, nil)
	n, err = s.Count()
	if err != nil || n != 2 {
		t.Fatalf("after 2 creates: Count()=%d err=%v", n, err)
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
