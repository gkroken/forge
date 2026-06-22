package server

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forge/internal/auth"
	forgeoidc "forge/internal/oidc"
)

// ── state cookie signing (unit) ───────────────────────────────────────────────

func TestOIDCStateRoundtrip(t *testing.T) {
	key := randomKey(t)
	state, nonce := "abc123", "xyz789"
	cookie := signOIDCState(key, state, nonce)
	got, ok := verifyOIDCState(key, cookie, state)
	if !ok {
		t.Fatal("expected verify to succeed")
	}
	if got != nonce {
		t.Fatalf("nonce: got %q, want %q", got, nonce)
	}
}

func TestOIDCStateVerify_WrongState(t *testing.T) {
	key := randomKey(t)
	cookie := signOIDCState(key, "real-state", "nonce")
	_, ok := verifyOIDCState(key, cookie, "attacker-state")
	if ok {
		t.Fatal("should not verify with wrong state")
	}
}

func TestOIDCStateVerify_TamperedSig(t *testing.T) {
	key := randomKey(t)
	cookie := signOIDCState(key, "state", "nonce")
	b := []byte(cookie)
	b[len(b)-1] ^= 0xFF
	_, ok := verifyOIDCState(key, string(b), "state")
	if ok {
		t.Fatal("should not verify tampered signature")
	}
}

func TestOIDCStateVerify_WrongKey(t *testing.T) {
	key1, key2 := randomKey(t), randomKey(t)
	cookie := signOIDCState(key1, "state", "nonce")
	_, ok := verifyOIDCState(key2, cookie, "state")
	if ok {
		t.Fatal("should not verify with different key")
	}
}

func TestOIDCStateVerify_Malformed(t *testing.T) {
	key := randomKey(t)
	for _, bad := range []string{"", "a.b", "a.b.c.d", "!!!.!!!.!!!"} {
		_, ok := verifyOIDCState(key, bad, "state")
		if ok {
			t.Fatalf("should reject malformed cookie %q", bad)
		}
	}
}

// ── HTTP handler tests ────────────────────────────────────────────────────────

func TestHandleOIDCLogin_RedirectsToIdP(t *testing.T) {
	srv, _, fake := newOIDCServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))

	if rw.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rw.Code)
	}
	loc := rw.Header().Get("Location")
	if !strings.HasPrefix(loc, fake.authURLPrefix) {
		t.Fatalf("redirect %q does not start with %q", loc, fake.authURLPrefix)
	}
}

func TestHandleOIDCLogin_SetsStateCookie(t *testing.T) {
	srv, _, _ := newOIDCServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))

	var found bool
	for _, c := range rw.Result().Cookies() {
		if c.Name == oidcStateCookie {
			found = true
			if c.HttpOnly != true {
				t.Error("state cookie must be HttpOnly")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Error("state cookie must be SameSite=Lax (required for IdP redirect-back)")
			}
			if c.MaxAge != oidcStateMaxAge {
				t.Errorf("state cookie MaxAge: got %d, want %d", c.MaxAge, oidcStateMaxAge)
			}
		}
	}
	if !found {
		t.Fatal("forge_oidc_state cookie not set")
	}
}

func TestHandleOIDCLogin_NotConfigured(t *testing.T) {
	srv := newAdminServer(t) // no OIDC, no auth
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when OIDC not configured, got %d", rw.Code)
	}
}

func TestHandleOIDCCallback_HappyPath(t *testing.T) {
	srv, authStore, fake := newOIDCServer(t)
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "u1", Email: "u1@example.com"}

	state, nonce := "test-state-1", "test-nonce-1"
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, callbackReq(t, srv, state, nonce, "fake-code", ""))

	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d\nbody: %s", rw.Code, rw.Body)
	}
	if rw.Header().Get("Location") != "/ui/" {
		t.Fatalf("expected redirect to /ui/, got %q", rw.Header().Get("Location"))
	}

	// forge_token cookie must be set and be a valid token in the store.
	var tokenSecret string
	for _, c := range rw.Result().Cookies() {
		if c.Name == auth.UISessionCookie {
			tokenSecret = c.Value
		}
	}
	if tokenSecret == "" {
		t.Fatal("forge_token cookie not set")
	}
	tok, err := authStore.Verify(tokenSecret)
	if err != nil || tok == nil {
		t.Fatalf("minted token is invalid: err=%v tok=%v", err, tok)
	}
	if !strings.HasPrefix(tok.Description, "oidc:") {
		t.Errorf("token description should start with 'oidc:', got %q", tok.Description)
	}
	if tok.ExpiresAt == nil {
		t.Error("OIDC token must have an expiry")
	}
}

func TestHandleOIDCCallback_TokenDescriptionUsesEmail(t *testing.T) {
	srv, authStore, fake := newOIDCServer(t)
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "opaque-sub", Email: "alice@example.com"}

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, callbackReq(t, srv, "s", "n", "code", ""))
	tokenSecret := sessionCookieValue(rw)
	tok, _ := authStore.Verify(tokenSecret)
	if tok.Description != "oidc:alice@example.com" {
		t.Errorf("description: got %q, want %q", tok.Description, "oidc:alice@example.com")
	}
}

func TestHandleOIDCCallback_TokenDescriptionFallsBackToSubject(t *testing.T) {
	srv, authStore, fake := newOIDCServer(t)
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "opaque-sub", Email: ""}

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, callbackReq(t, srv, "s", "n", "code", ""))
	tokenSecret := sessionCookieValue(rw)
	tok, _ := authStore.Verify(tokenSecret)
	if tok.Description != "oidc:opaque-sub" {
		t.Errorf("description: got %q, want %q", tok.Description, "oidc:opaque-sub")
	}
}

func TestHandleOIDCCallback_MintedTokenHasDefaultGrants(t *testing.T) {
	srv, authStore, fake := newOIDCServer(t)
	fake.defaultGrants = []auth.Grant{{Repo: "npm-hosted", Role: auth.RoleWrite}}
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "u1"}

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, callbackReq(t, srv, "s", "n", "code", ""))
	tokenSecret := sessionCookieValue(rw)
	tok, _ := authStore.Verify(tokenSecret)
	if len(tok.Grants) != 1 || tok.Grants[0].Repo != "npm-hosted" || tok.Grants[0].Role != auth.RoleWrite {
		t.Errorf("grants: got %+v", tok.Grants)
	}
}

func TestHandleOIDCCallback_GroupMapsToAdmin(t *testing.T) {
	srv, authStore, fake := newOIDCServer(t)
	srv.GroupMapper = auth.NewGroupRoleMapper([]auth.GroupRule{
		{Group: "forge-admins", Role: auth.RoleAdmin},
		{Group: "devs", Role: auth.RoleWrite},
	})
	fake.defaultGrants = []auth.Grant{{Repo: "*", Role: auth.RoleRead}}
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "u1", Email: "a@x.com", Groups: []string{"devs", "forge-admins"}}

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, callbackReq(t, srv, "s", "n", "code", ""))
	tok, _ := authStore.Verify(sessionCookieValue(rw))
	if tok == nil || len(tok.Grants) != 1 || tok.Grants[0].Repo != "*" || tok.Grants[0].Role != auth.RoleAdmin {
		t.Fatalf("expected admin grant on *, got %+v", tok)
	}
}

func TestHandleOIDCCallback_NoGroupMatchUsesFallback(t *testing.T) {
	srv, authStore, fake := newOIDCServer(t)
	srv.GroupMapper = auth.NewGroupRoleMapper([]auth.GroupRule{{Group: "forge-admins", Role: auth.RoleAdmin}})
	fake.defaultGrants = []auth.Grant{{Repo: "npm-hosted", Role: auth.RoleRead}}
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "u1", Groups: []string{"contractors"}}

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, callbackReq(t, srv, "s", "n", "code", ""))
	tok, _ := authStore.Verify(sessionCookieValue(rw))
	if tok == nil || len(tok.Grants) != 1 || tok.Grants[0].Repo != "npm-hosted" || tok.Grants[0].Role != auth.RoleRead {
		t.Fatalf("expected fallback grant, got %+v", tok)
	}
}

func TestHandleOIDCCallback_ProvisionsUser(t *testing.T) {
	srv, _, fake := newOIDCServer(t)
	srv.Users = auth.NewUserStore(srv.Meta)
	srv.GroupMapper = auth.NewGroupRoleMapper([]auth.GroupRule{{Group: "forge-admins", Role: auth.RoleAdmin}})
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "u1", Email: "alice@x.com", Groups: []string{"forge-admins"}}

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, callbackReq(t, srv, "s", "n", "code", ""))

	u, ok, _ := srv.Users.Get("alice@x.com")
	if !ok {
		t.Fatal("SSO user was not provisioned")
	}
	if auth.BaseRoleFor(u.Role) != auth.RoleAdmin {
		t.Errorf("provisioned role: got %q, want admin", u.Role)
	}
	if u.LastLogin == nil {
		t.Error("LastLogin should be set on SSO login")
	}
}

func TestHandleOIDCCallback_DisabledUserDenied(t *testing.T) {
	srv, authStore, fake := newOIDCServer(t)
	srv.Users = auth.NewUserStore(srv.Meta)
	// Pre-create then disable the user.
	if err := srv.Users.Upsert(auth.User{Username: "blocked@x.com", Role: "read"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Users.SetDisabled("blocked@x.com", true); err != nil {
		t.Fatal(err)
	}
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "u1", Email: "blocked@x.com"}

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, callbackReq(t, srv, "s", "n", "code", ""))

	if loc := rw.Header().Get("Location"); !strings.Contains(loc, "error=disabled") {
		t.Fatalf("expected error=disabled redirect, got %q", loc)
	}
	if sessionCookieValue(rw) != "" {
		t.Error("no session cookie should be set for a disabled user")
	}
	if n, _ := authStore.Count(); n != 0 {
		t.Errorf("no token should be minted for a disabled user, got %d", n)
	}
}

func TestHandleOIDCCallback_IdPErrorRedirectsToLogin(t *testing.T) {
	srv, _, _ := newOIDCServer(t)
	rw := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?error=access_denied&error_description=user+denied", nil)
	srv.Routes().ServeHTTP(rw, r)

	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rw.Code)
	}
	if !strings.Contains(rw.Header().Get("Location"), "error=oidc") {
		t.Fatalf("expected error=oidc in redirect, got %q", rw.Header().Get("Location"))
	}
}

func TestHandleOIDCCallback_MissingStateCookie(t *testing.T) {
	srv, _, _ := newOIDCServer(t)
	rw := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=x&code=y", nil)
	srv.Routes().ServeHTTP(rw, r)

	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rw.Code)
	}
	if !strings.Contains(rw.Header().Get("Location"), "error=invalid") {
		t.Fatalf("expected error=invalid in redirect, got %q", rw.Header().Get("Location"))
	}
}

func TestHandleOIDCCallback_StateMismatch(t *testing.T) {
	srv, _, _ := newOIDCServer(t)
	// Cookie signed for "real-state" but URL has "different-state".
	cookieVal := signOIDCState(srv.oidcKey, "real-state", "nonce")
	rw := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=different-state&code=y", nil)
	r.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: cookieVal})
	srv.Routes().ServeHTTP(rw, r)

	if !strings.Contains(rw.Header().Get("Location"), "error=invalid") {
		t.Fatalf("expected error=invalid in redirect, got %q", rw.Header().Get("Location"))
	}
}

func TestHandleOIDCCallback_MissingCode(t *testing.T) {
	srv, _, _ := newOIDCServer(t)
	state := "st"
	cookieVal := signOIDCState(srv.oidcKey, state, "nonce")
	rw := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state="+state, nil) // no code
	r.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: cookieVal})
	srv.Routes().ServeHTTP(rw, r)

	if !strings.Contains(rw.Header().Get("Location"), "error=invalid") {
		t.Fatalf("expected error=invalid in redirect, got %q", rw.Header().Get("Location"))
	}
}

func TestHandleOIDCCallback_ExchangeError(t *testing.T) {
	srv, _, fake := newOIDCServer(t)
	fake.exchangeErr = context.DeadlineExceeded

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, callbackReq(t, srv, "s", "n", "code", ""))

	if !strings.Contains(rw.Header().Get("Location"), "error=oidc") {
		t.Fatalf("expected error=oidc in redirect, got %q", rw.Header().Get("Location"))
	}
}

func TestHandleOIDCCallback_ClearsStateCookie(t *testing.T) {
	srv, _, fake := newOIDCServer(t)
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "u1"}

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, callbackReq(t, srv, "st", "nn", "code", ""))

	for _, c := range rw.Result().Cookies() {
		if c.Name == oidcStateCookie {
			if c.MaxAge != -1 {
				t.Errorf("state cookie MaxAge should be -1 (delete), got %d", c.MaxAge)
			}
		}
	}
}

func TestHandleOIDCCallback_NotConfigured(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when OIDC not configured, got %d", rw.Code)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// fakeOIDCProvider is a controllable implementation of oidcProvider for tests.
type fakeOIDCProvider struct {
	authURLPrefix string
	exchangeInfo  forgeoidc.UserInfo
	exchangeErr   error
	defaultGrants []auth.Grant
	tokenTTL      time.Duration
}

func (f *fakeOIDCProvider) AuthURL(state, nonce string) string {
	return f.authURLPrefix + "?state=" + state + "&nonce=" + nonce
}

func (f *fakeOIDCProvider) Exchange(_ context.Context, _, _ string) (forgeoidc.UserInfo, error) {
	return f.exchangeInfo, f.exchangeErr
}

func (f *fakeOIDCProvider) DefaultGrants() []auth.Grant {
	if f.defaultGrants != nil {
		return f.defaultGrants
	}
	return []auth.Grant{{Repo: "*", Role: auth.RoleRead}}
}

func (f *fakeOIDCProvider) TokenTTL() time.Duration {
	if f.tokenTTL != 0 {
		return f.tokenTTL
	}
	return 8 * time.Hour
}

func (f *fakeOIDCProvider) Issuer() string      { return "https://idp.example.com" }
func (f *fakeOIDCProvider) ClientID() string    { return "forge" }
func (f *fakeOIDCProvider) RedirectURL() string { return "https://forge.example.com/auth/oidc/callback" }
func (f *fakeOIDCProvider) GroupsClaim() string { return "groups" }

// newOIDCServer returns an auth-enabled server wired with a fakeOIDCProvider.
func newOIDCServer(t *testing.T) (*Server, auth.Store, *fakeOIDCProvider) {
	t.Helper()
	srv, authStore := newAuthServer(t)
	fake := &fakeOIDCProvider{authURLPrefix: "https://idp.example.com/authorize"}
	srv.OIDC = fake
	srv.oidcKey = randomKey(t)
	return srv, authStore, fake
}

// callbackReq builds a GET /auth/oidc/callback request with a valid signed
// state cookie so the handler's CSRF check passes.
func callbackReq(t *testing.T, srv *Server, state, nonce, code, extra string) *http.Request {
	t.Helper()
	u := "/auth/oidc/callback?state=" + state + "&code=" + code + extra
	r := httptest.NewRequest(http.MethodGet, u, nil)
	r.AddCookie(&http.Cookie{
		Name:  oidcStateCookie,
		Value: signOIDCState(srv.oidcKey, state, nonce),
	})
	return r
}

// sessionCookieValue extracts the forge_token cookie value from a response.
func sessionCookieValue(rw *httptest.ResponseRecorder) string {
	for _, c := range rw.Result().Cookies() {
		if c.Name == auth.UISessionCookie {
			return c.Value
		}
	}
	return ""
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}
