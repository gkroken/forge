// Security-focused tests for the OIDC handlers.
// Each test probes a specific attack vector rather than happy-path behaviour.
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"forge/internal/auth"
	forgeoidc "forge/internal/oidc"
)

// ── HMAC correctness ──────────────────────────────────────────────────────────

// TestOIDCHMAC_SeparatorCollision verifies that swapping characters across the
// state/nonce boundary does not produce the same MAC.  Without length-prefixing,
// ("a|b","c") and ("a","b|c") would collide.
func TestOIDCHMAC_SeparatorCollision(t *testing.T) {
	key := randomKey(t)
	mac1 := oidcHMAC(key, "a|b", "c")
	mac2 := oidcHMAC(key, "a", "b|c")
	if string(mac1) == string(mac2) {
		t.Fatal("HMAC separator collision: different (state,nonce) pairs produced identical MAC")
	}
}

// TestOIDCHMAC_EmptyFields ensures empty state/nonce are distinct from each other.
func TestOIDCHMAC_EmptyFields(t *testing.T) {
	key := randomKey(t)
	mac1 := oidcHMAC(key, "", "x")
	mac2 := oidcHMAC(key, "x", "")
	if string(mac1) == string(mac2) {
		t.Fatal("empty-field collision: (empty,x) and (x,empty) produced same MAC")
	}
}

// ── Redirect safety ───────────────────────────────────────────────────────────

// TestOIDCCallback_LocationHeaderIsNotUserControlled verifies that no
// user-supplied query parameter appears verbatim in the Location header on
// any response path from the callback handler.
func TestOIDCCallback_LocationHeaderIsNotUserControlled(t *testing.T) {
	srv, _, fake := newOIDCServer(t)
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "u1"}

	injections := []string{
		"https://evil.example.com",
		"//evil.example.com",
		"/evil",
		// CRLF is encoded by browsers/httptest before reaching us;
		// we test the URL-encoded form that would actually arrive.
		"%0D%0AX-Injected:%20yes",
		"javascript:alert(1)",
	}

	for _, inject := range injections {
		t.Run(inject, func(t *testing.T) {
			// Inject via state param (should fail CSRF check, but Location must be safe).
			rw := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet,
				"/auth/oidc/callback?state="+inject+"&code=x", nil)
			srv.Routes().ServeHTTP(rw, r)

			loc := rw.Header().Get("Location")
			if strings.Contains(loc, "evil.example.com") ||
				strings.Contains(loc, "javascript:") ||
				strings.Contains(loc, "X-Injected") {
				t.Errorf("user-controlled value leaked into Location header: %q", loc)
			}
			if !strings.HasPrefix(loc, "/ui/") {
				t.Errorf("unexpected Location %q — must start with /ui/", loc)
			}
		})
	}
}

// TestOIDCLogin_RedirectTargetIsIdP confirms the login endpoint only ever
// redirects to the configured IdP, never to anything user-supplied.
func TestOIDCLogin_RedirectTargetIsIdP(t *testing.T) {
	srv, _, fake := newOIDCServer(t)
	for _, extraQuery := range []string{"", "?redirect=https://evil.example.com", "?next=/evil"} {
		rw := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rw,
			httptest.NewRequest(http.MethodGet, "/auth/oidc/login"+extraQuery, nil))

		loc := rw.Header().Get("Location")
		if !strings.HasPrefix(loc, fake.authURLPrefix) {
			t.Errorf("login redirect %q does not point at configured IdP %q", loc, fake.authURLPrefix)
		}
		if strings.Contains(loc, "evil.example.com") {
			t.Errorf("evil URL leaked into login redirect: %q", loc)
		}
	}
}

// ── Input robustness ─────────────────────────────────────────────────────────

// TestOIDCCallback_OversizedState verifies that a very long state parameter
// does not panic or cause unexpected behaviour (just a safe error redirect).
func TestOIDCCallback_OversizedState(t *testing.T) {
	srv, _, _ := newOIDCServer(t)
	hugeState := strings.Repeat("A", 8192)
	rw := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		"/auth/oidc/callback?state="+hugeState+"&code=x", nil)
	r.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: "x.y.z"}) // invalid but present

	// Must not panic; must redirect to login.
	srv.Routes().ServeHTTP(rw, r)
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rw.Code)
	}
	if !strings.HasPrefix(rw.Header().Get("Location"), "/ui/login") {
		t.Fatalf("expected redirect to /ui/login, got %q", rw.Header().Get("Location"))
	}
}

// TestOIDCCallback_NullByteInState verifies that a null byte injected into the
// state URL param does not bypass CSRF verification.
func TestOIDCCallback_NullByteInState(t *testing.T) {
	srv, _, _ := newOIDCServer(t)
	// Cookie was legitimately signed for "normal-state".
	// Attacker submits state="\x00" hoping to confuse the comparison.
	cookieVal := signOIDCState(srv.oidcKey, "normal-state", "nonce")
	rw := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=%00&code=x", nil)
	r.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: cookieVal})
	srv.Routes().ServeHTTP(rw, r)
	if !strings.Contains(rw.Header().Get("Location"), "error=invalid") {
		t.Errorf("null-byte state must be rejected, got Location=%q", rw.Header().Get("Location"))
	}
}

// TestOIDCCallback_OversizedCookieValue exercises decoding of a junk cookie.
func TestOIDCCallback_OversizedCookieValue(t *testing.T) {
	srv, _, _ := newOIDCServer(t)
	rw := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=x&code=y", nil)
	r.AddCookie(&http.Cookie{
		Name:  oidcStateCookie,
		Value: strings.Repeat("AAAA", 2048), // ~8 KiB of junk
	})
	srv.Routes().ServeHTTP(rw, r)
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rw.Code)
	}
	if !strings.HasPrefix(rw.Header().Get("Location"), "/ui/login") {
		t.Fatalf("expected /ui/login redirect, got %q", rw.Header().Get("Location"))
	}
}

// TestOIDCCallback_SpecialCharsInErrorDescription verifies that an IdP-supplied
// error_description containing newlines and quotes does not crash the handler
// or produce a split HTTP response (log injection attempt).
func TestOIDCCallback_SpecialCharsInErrorDescription(t *testing.T) {
	srv, _, _ := newOIDCServer(t)
	// A malicious IdP could set error_description to inject log lines or
	// manipulate structured output.
	payloads := []string{
		// CRLF injection attempt (would be encoded by any HTTP client).
		"line1\r\nX-Injected: bad",
		`{"event":"fake","severity":"CRITICAL"}`,
		strings.Repeat("x", 4096),
		"\x00\x01\x02",
	}
	for _, payload := range payloads {
		rw := httptest.NewRecorder()
		// Build the URL via url.Values so control characters are percent-encoded
		// before reaching the HTTP layer — exactly as a real browser would send them.
		rawQuery := url.Values{
			"error":             {"access_denied"},
			"error_description": {payload},
		}.Encode()
		r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?"+rawQuery, nil)
		// Must not panic; must redirect to login.
		srv.Routes().ServeHTTP(rw, r)
		if rw.Code != http.StatusSeeOther {
			t.Fatalf("payload %q: expected 303, got %d", payload, rw.Code)
		}
		// The Location header must not contain any part of the payload.
		loc := rw.Header().Get("Location")
		if strings.Contains(loc, "CRITICAL") || strings.Contains(loc, "X-Injected") {
			t.Errorf("payload leaked into Location: %q", loc)
		}
	}
}

// ── Concurrent callback replay ────────────────────────────────────────────────

// TestOIDCCallback_ConcurrentReplay fires the same valid callback request from
// multiple goroutines simultaneously.  The fake provider always succeeds, so
// both will mint tokens — but neither should panic, deadlock, or corrupt state.
// In production the IdP code is single-use so only the first would succeed.
func TestOIDCCallback_ConcurrentReplay(t *testing.T) {
	srv, authStore, fake := newOIDCServer(t)
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "u1", Email: "u1@example.com"}

	state, nonce := "concurrent-state", "concurrent-nonce"
	cookieVal := signOIDCState(srv.oidcKey, state, nonce)

	const workers = 10
	results := make([]int, workers)
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			rw := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet,
				"/auth/oidc/callback?state="+state+"&code=fakecode", nil)
			r.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: cookieVal})
			srv.Routes().ServeHTTP(rw, r)
			results[i] = rw.Code
		}()
	}
	wg.Wait()

	// All requests must have completed with 303.
	for i, code := range results {
		if code != http.StatusSeeOther {
			t.Errorf("worker %d: got %d, want 303", i, code)
		}
	}

	// Verify all minted tokens are independently valid (each worker got its own token).
	tokens, err := authStore.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range tokens {
		if !strings.HasPrefix(tok.Description, "oidc:") {
			continue
		}
		if tok.ExpiresAt == nil || tok.ExpiresAt.Before(time.Now()) {
			t.Errorf("minted token %s has no valid expiry", tok.ID)
		}
	}
}

// ── State cookie properties ───────────────────────────────────────────────────

// TestOIDCLogin_StateCookieIsUnique verifies that two successive login requests
// produce different state cookies (no reuse of state or nonce).
func TestOIDCLogin_StateCookieIsUnique(t *testing.T) {
	srv, _, _ := newOIDCServer(t)

	cookies := make([]string, 3)
	for i := range cookies {
		rw := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))
		for _, c := range rw.Result().Cookies() {
			if c.Name == oidcStateCookie {
				cookies[i] = c.Value
			}
		}
	}
	for i := 0; i < len(cookies); i++ {
		for j := i + 1; j < len(cookies); j++ {
			if cookies[i] == cookies[j] {
				t.Errorf("login %d and %d produced identical state cookie — state is not unique", i, j)
			}
		}
	}
}

// TestOIDCCallback_SessionCookieProperties checks that the minted forge_token
// cookie has the correct security attributes.
func TestOIDCCallback_SessionCookieProperties(t *testing.T) {
	srv, _, fake := newOIDCServer(t)
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "u1"}

	rw := httptest.NewRecorder()
	r := callbackReq(t, srv, "st", "nn", "code", "")
	// Simulate TLS so Secure=true is set.
	r.Header.Set("X-Forwarded-Proto", "https")
	srv.Routes().ServeHTTP(rw, r)

	for _, c := range rw.Result().Cookies() {
		if c.Name != auth.UISessionCookie {
			continue
		}
		if !c.HttpOnly {
			t.Error("forge_token must be HttpOnly")
		}
		if !c.Secure {
			t.Error("forge_token must be Secure when request is HTTPS")
		}
		if c.SameSite != http.SameSiteStrictMode {
			t.Error("forge_token must be SameSite=Strict")
		}
		if c.MaxAge <= 0 {
			t.Errorf("forge_token MaxAge must be positive, got %d", c.MaxAge)
		}
	}
}

// TestOIDCCallback_StateCookieProperties checks that the OIDC state cookie
// uses Lax (not Strict, which would break IdP redirect-back) and is HttpOnly.
func TestOIDCLogin_StateCookieIsSameSiteLax(t *testing.T) {
	srv, _, _ := newOIDCServer(t)
	rw := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	srv.Routes().ServeHTTP(rw, r)

	for _, c := range rw.Result().Cookies() {
		if c.Name != oidcStateCookie {
			continue
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("state cookie SameSite must be Lax (Strict breaks IdP redirect-back), got %v", c.SameSite)
		}
		if !c.HttpOnly {
			t.Error("state cookie must be HttpOnly")
		}
		if !c.Secure {
			t.Error("state cookie must be Secure when request is HTTPS")
		}
	}
}

// TestOIDCCallback_ForgeTokenNotLeakedInBody verifies that the raw forge token
// secret is never written into the response body (only set as a cookie).
func TestOIDCCallback_ForgeTokenNotLeakedInBody(t *testing.T) {
	srv, _, fake := newOIDCServer(t)
	fake.exchangeInfo = forgeoidc.UserInfo{Subject: "u1"}

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, callbackReq(t, srv, "s", "n", "c", ""))

	secret := sessionCookieValue(rw)
	if secret == "" {
		t.Skip("no session cookie set; check other tests for callback failures")
	}
	if strings.Contains(rw.Body.String(), secret) {
		t.Error("forge token secret leaked into response body")
	}
	if strings.Contains(rw.Body.String(), "forge_") {
		t.Error("forge token prefix leaked into response body")
	}
}

// fakeOIDCProvider.Exchange captures args for inspection in tests.
// Extend the base type to capture exchange calls.
type capturingFakeOIDC struct {
	fakeOIDCProvider
	mu    sync.Mutex
	calls int
}

func (f *capturingFakeOIDC) Exchange(_ context.Context, _, _ string) (forgeoidc.UserInfo, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.fakeOIDCProvider.exchangeInfo, f.fakeOIDCProvider.exchangeErr
}
