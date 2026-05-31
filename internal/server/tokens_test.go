package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

// newAuthServer builds a Server with auth enabled backed by temp FS stores.
func newAuthServer(t *testing.T) (*Server, auth.Store) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	authStore := auth.NewMetaStore(m)
	srv := New(repo.NewManager(), format.NewRegistry(), b, m, authStore)
	return srv, authStore
}

func tokenReq(t *testing.T, method, path, bearer, body string) *http.Request {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

// TestTokens_AuthNotEnabled verifies the API returns 501 when auth is off.
func TestTokens_AuthNotEnabled(t *testing.T) {
	srv := newAdminServer(t) // nil auth store = eval mode
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/tokens", nil))
	if rw.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", rw.Code)
	}
}

// TestTokens_Bootstrap creates the first token without authentication.
func TestTokens_Bootstrap(t *testing.T) {
	srv, _ := newAuthServer(t)
	body := `{"description":"admin","grants":[{"repo":"*","role":3}]}`
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodPost, "/api/v1/tokens", "", body))
	if rw.Code != http.StatusCreated {
		t.Fatalf("bootstrap create: got %d\n%s", rw.Code, rw.Body)
	}
	var resp struct {
		Secret string `json:"secret"`
		ID     string `json:"id"`
	}
	json.NewDecoder(rw.Body).Decode(&resp)
	if resp.Secret == "" || resp.ID == "" {
		t.Fatalf("expected non-empty secret and id: %+v", resp)
	}
}

// TestTokens_List and Revoke exercise the full CRUD flow.
func TestTokens_ListAndRevoke(t *testing.T) {
	srv, authStore := newAuthServer(t)

	// Seed a bootstrap admin token directly so we have credentials.
	_, secret, err := authStore.Create("admin", []auth.Grant{{Repo: "*", Role: auth.RoleAdmin}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Create a second token via the API.
	body := `{"description":"ci-bot","grants":[{"repo":"helm-hosted","role":1}]}`
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodPost, "/api/v1/tokens", secret, body))
	if rw.Code != http.StatusCreated {
		t.Fatalf("create second token: got %d\n%s", rw.Code, rw.Body)
	}
	var created struct{ ID string `json:"id"` }
	json.NewDecoder(rw.Body).Decode(&created)

	// List tokens — must include both.
	rw = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodGet, "/api/v1/tokens", secret, ""))
	if rw.Code != http.StatusOK {
		t.Fatalf("list: got %d", rw.Code)
	}
	var tokens []auth.Token
	json.NewDecoder(rw.Body).Decode(&tokens)
	if len(tokens) < 2 {
		t.Fatalf("expected ≥2 tokens, got %d", len(tokens))
	}

	// Revoke the second token.
	rw = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodDelete, "/api/v1/tokens/"+created.ID, secret, ""))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("revoke: got %d\n%s", rw.Code, rw.Body)
	}
}

// TestTokens_RouteNotFound covers the default case in handleTokens.
func TestTokens_RouteNotFound(t *testing.T) {
	srv, _ := newAuthServer(t)
	// Seed a token so we're past bootstrap.
	_, secret, _ := srv.Auth.Create("admin", []auth.Grant{{Repo: "*", Role: auth.RoleAdmin}}, nil)

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodPatch, "/api/v1/tokens/someid", secret, ""))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

// TestTokens_BadBody covers JSON decode failure in createToken.
func TestTokens_BadBody(t *testing.T) {
	srv, _ := newAuthServer(t)
	rw := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader("not json"))
	r.Header.Set("Content-Type", "application/json")
	srv.Routes().ServeHTTP(rw, r)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rw.Code)
	}
}

// TestTokens_ListRequiresAdmin verifies that a non-admin token gets 403 on list.
func TestTokens_ListRequiresAdmin(t *testing.T) {
	srv, authStore := newAuthServer(t)
	_, adminSecret, _ := authStore.Create("admin", []auth.Grant{{Repo: "*", Role: auth.RoleAdmin}}, nil)
	_ = adminSecret // needed to move past bootstrap so the next create requires admin
	_, readSecret, _ := authStore.Create("reader", []auth.Grant{{Repo: "x", Role: auth.RoleRead}}, nil)

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodGet, "/api/v1/tokens", readSecret, ""))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("non-admin list: expected 403, got %d", rw.Code)
	}
}

// TestTokens_RevokeRequiresAdmin verifies that a non-admin token gets 403 on revoke.
func TestTokens_RevokeRequiresAdmin(t *testing.T) {
	srv, authStore := newAuthServer(t)
	tok, adminSecret, _ := authStore.Create("admin", []auth.Grant{{Repo: "*", Role: auth.RoleAdmin}}, nil)
	_ = adminSecret
	_, readSecret, _ := authStore.Create("reader", []auth.Grant{{Repo: "x", Role: auth.RoleRead}}, nil)

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, tokenReq(t, http.MethodDelete, "/api/v1/tokens/"+tok.ID, readSecret, ""))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("non-admin revoke: expected 403, got %d", rw.Code)
	}
}

// TestServer_OCI_BaseEndpoint covers handleOCI's /v2/ base check.
func TestServer_OCI_BaseEndpoint(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 for /v2/, got %d", rw.Code)
	}
	if rw.Header().Get("OCI-Distribution-Spec-Version") == "" {
		t.Fatal("expected OCI-Distribution-Spec-Version header")
	}
}
